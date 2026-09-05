package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

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
