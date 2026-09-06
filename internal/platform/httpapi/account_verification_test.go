package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/audit"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestAccountAPIAutomaticR2Credentials(t *testing.T) {
	t.Parallel()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{21}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	cloudflare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "rejected") {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":1000,"message":"invalid rejected-token"}]}`)
			return
		}
		fmt.Fprint(w, `{"success":true,"result":{"id":"0123456789abcdef0123456789abcdef","status":"active"}}`)
	}))
	defer cloudflare.Close()
	store := accounts.NewStore(db, secret.NewRepository(db, cipher))
	store.Verifier = accounts.Verifier{BaseURL: cloudflare.URL, Client: cloudflare.Client()}
	api := &API{deps: Dependencies{Accounts: store, Jobs: jobs.NewStore(db), Audit: audit.NewStore(db)}}
	created := httptest.NewRecorder()
	api.createAccount(created, httptest.NewRequest(http.MethodPost, "/api/v1/accounts", strings.NewReader(`{"name":"automatic","cloudflare_account_id":"account","api_token":"first-token","r2_from_api_token":true}`)))
	if created.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	var result struct {
		Account   accounts.Account `json:"account"`
		Scheduled bool             `json:"verification_scheduled"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Account.R2FromAPIToken || !result.Account.HasR2Credentials || result.Account.Verification == nil || strings.Contains(created.Body.String(), "first-token") {
		t.Fatal("missing automatic credentials or verification, or secret exposed")
	}
	id := result.Account.ID
	patch := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPatch, "/api/v1/accounts/"+id+"/credentials", strings.NewReader(body))
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		api.updateAccountCredentials(w, r)
		return w
	}
	updated := patch(`{"api_token":"next-token"}`)
	if updated.Code != http.StatusAccepted {
		t.Fatalf("update: %d %s", updated.Code, updated.Body)
	}
	if err := json.Unmarshal(updated.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Scheduled || !result.Account.R2FromAPIToken {
		t.Fatal("rotation did not retain automatic mode and schedule verification")
	}
	stored, err := store.Get(context.Background(), id, true)
	if err != nil || stored.APIToken != "next-token" || stored.R2SecretAccessKey != fmt.Sprintf("%x", sha256.Sum256([]byte("next-token"))) {
		t.Fatal("rotation did not persist matching pair")
	}
	var detail string
	if err := db.QueryRow(`SELECT detail_json FROM audit_events WHERE action='account.credentials.update' ORDER BY created_at DESC LIMIT 1`).Scan(&detail); err != nil || !strings.Contains(detail, `"r2_credentials_action":"derived"`) {
		t.Fatalf("derived update missing from audit: %s %v", detail, err)
	}
	failed := patch(`{"api_token":"rejected-token"}`)
	if failed.Code != http.StatusBadGateway || !strings.Contains(failed.Body.String(), "r2_derivation_failed") || strings.Contains(failed.Body.String(), "rejected-token") {
		t.Fatalf("unsafe failure response: %d %s", failed.Code, failed.Body)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM jobs").Scan(&count); err != nil || count != 2 {
		t.Fatal("failed update scheduled a job")
	}
	current, err := store.Get(context.Background(), id, true)
	if err != nil || current.APIToken != stored.APIToken || current.R2SecretAccessKey != stored.R2SecretAccessKey {
		t.Fatal("failed update changed credentials")
	}
	if response := patch(`{"r2_from_api_token":true,"clear_r2_credentials":true}`); response.Code != http.StatusBadRequest {
		t.Fatal("conflicting modes accepted")
	}
}

func TestAccountCreationAutomaticallyRunsOneVerification(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, rejectedPath, health, detail string
	}{
		{name: "healthy", health: "healthy"},
		{name: "token rejected", rejectedPath: "tokens/verify", health: "error", detail: "HTTP 403"},
		{name: "R2 permission missing", rejectedPath: "r2/buckets", health: "degraded", detail: "R2 桶列表读取"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			cipher, err := secret.NewCipher(bytes.Repeat([]byte{23}, secret.KeySize))
			if err != nil {
				t.Fatal(err)
			}
			store := accounts.NewStore(db, secret.NewRepository(db, cipher))
			jobStore := jobs.NewStore(db)
			api := &API{deps: Dependencies{Accounts: store, Jobs: jobStore}}
			created := httptest.NewRecorder()
			api.createAccount(created, httptest.NewRequest(http.MethodPost, "/api/v1/accounts",
				strings.NewReader(`{"name":"primary","cloudflare_account_id":"account","api_token":"private-token"}`)))
			if created.Code != http.StatusAccepted {
				t.Fatalf("create = %d: %s", created.Code, created.Body)
			}
			var result struct {
				Account accounts.Account `json:"account"`
				Job     jobs.Job         `json:"job"`
			}
			if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Account.Verification == nil || result.Account.Verification.Status != "pending" || result.Account.Verification.JobID != result.Job.ID {
				t.Fatalf("create missing pending verification: %s", created.Body)
			}
			readAccount := func() accounts.Account {
				t.Helper()
				response := httptest.NewRecorder()
				api.listAccounts(response, httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil))
				var payload struct {
					Accounts []accounts.Account `json:"accounts"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || len(payload.Accounts) != 1 {
					t.Fatalf("list accounts = %s, error = %v", response.Body, err)
				}
				return payload.Accounts[0]
			}
			if account := readAccount(); account.Verification == nil || account.Verification.Status != "pending" {
				t.Fatalf("list missing queued check: %#v", account.Verification)
			}

			cloudflare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.rejectedPath != "" && strings.Contains(r.URL.Path, test.rejectedPath) {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"success":true,"result":{"status":"active"}}`))
			}))
			defer cloudflare.Close()
			handler := accounts.CapabilityJobHandler{Store: store, Verifier: accounts.Verifier{BaseURL: cloudflare.URL, Client: cloudflare.Client()}}
			runner := jobs.NewRunner(jobStore)
			runner.Poll = 5 * time.Millisecond
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			runner.Register(accounts.CapabilityJobType, func(ctx context.Context, job jobs.Job) error {
				if calls.Add(1) == 1 {
					close(started)
				}
				select {
				case <-release:
					return handler.Handle(ctx, job)
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("new account was not automatically verified")
			}
			if account := readAccount(); account.Verification == nil || account.Verification.Status != "running" {
				t.Fatalf("list missing running check: %#v", account.Verification)
			}
			close(release)
			deadline := time.Now().Add(5 * time.Second)
			for {
				account := readAccount()
				if account.Verification == nil {
					if account.HealthStatus != test.health || !strings.Contains(account.HealthError, test.detail) || len(account.Capabilities) != 5 {
						t.Fatalf("incorrect verification result: %#v", account)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("verification did not finish")
				}
				time.Sleep(5 * time.Millisecond)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type = ?`, accounts.CapabilityJobType).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 || calls.Load() != 1 {
				t.Fatalf("expected one automatic detection: jobs = %d, executions = %d", count, calls.Load())
			}
		})
	}
}
