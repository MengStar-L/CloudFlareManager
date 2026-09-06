package accounts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

const testTokenID = "0123456789abcdef0123456789abcdef"

func tokenTestStore(t *testing.T, handler http.HandlerFunc) *Store {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{19}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	store := NewStore(db, secret.NewRepository(db, cipher))
	store.Verifier = Verifier{BaseURL: server.URL, Client: server.Client()}
	return store
}

func activeToken(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintf(w, `{"success":true,"result":{"id":%q,"status":"active"}}`, testTokenID)
}

func TestDeriveR2VerifiesTokenIdentityAndRedactsFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, response         string
		accountOnly, wantError bool
	}{
		{name: "user token", response: `{"id":"` + testTokenID + `","status":"active"}`},
		{name: "account token", accountOnly: true, response: `{"id":"` + testTokenID + `","status":"active"}`},
		{name: "expired token", wantError: true, response: `{"id":"` + testTokenID + `","status":"expired"}`},
		{name: "missing status", wantError: true, response: `{"id":"` + testTokenID + `"}`},
		{name: "missing ID", wantError: true, response: `{"status":"active"}`},
		{name: "invalid ID", wantError: true, response: `{"id":"../token","status":"active"}`},
		{name: "rejected", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var paths []string
			store := tokenTestStore(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.Header.Get("Authorization") != "Bearer private-token" {
					t.Error("incorrect authorization")
				}
				if test.response == "" || (test.accountOnly && r.URL.Path == "/user/tokens/verify") {
					w.WriteHeader(401)
					fmt.Fprint(w, `{"success":false,"errors":[{"code":1000,"message":"invalid private-token"}]}`)
					return
				}
				fmt.Fprintf(w, `{"success":true,"result":%s}`, test.response)
			})
			access, key, err := store.Verifier.deriveR2Credentials(context.Background(), "account", "private-token")
			if test.wantError {
				var derivedErr *CredentialDerivationError
				if !errors.As(err, &derivedErr) || strings.Contains(err.Error(), "private-token") || access != "" || key != "" {
					t.Fatalf("unsafe or missing derivation failure: %v", err)
				}
			} else {
				if err != nil || access != testTokenID || key != fmt.Sprintf("%x", sha256.Sum256([]byte("private-token"))) {
					t.Fatalf("derivation mismatch: %v", err)
				}
				if test.accountOnly && (len(paths) != 2 || paths[1] != "/accounts/account/tokens/verify") {
					t.Fatalf("incorrect fallback paths: %v", paths)
				}
			}
		})
	}
}

func TestAutomaticR2CredentialsFollowTokenRotationAndManualMode(t *testing.T) {
	t.Parallel()
	store := tokenTestStore(t, activeToken)
	ctx := context.Background()
	created, err := store.Create(ctx, CreateInput{Name: "auto", CloudflareAccountID: "account", APIToken: " token-v1 ", R2FromAPIToken: true})
	if err != nil {
		t.Fatal(err)
	}
	check := func(token string, auto bool, expectedKey string) Account {
		t.Helper()
		account, err := store.Get(ctx, created.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if account.APIToken != token || account.R2FromAPIToken != auto || !account.HasR2Credentials || account.R2SecretAccessKey != expectedKey {
			t.Fatal("credential state mismatch")
		}
		data, err := json.Marshal(account)
		if err != nil || bytes.Contains(data, []byte(expectedKey)) || bytes.Contains(data, []byte(token)) {
			t.Fatal("secret exposed in account JSON")
		}
		return account
	}
	keyV1 := fmt.Sprintf("%x", sha256.Sum256([]byte("token-v1")))
	check("token-v1", true, keyV1)
	token := "token-v2"
	if _, err := store.UpdateCredentials(ctx, created.ID, UpdateCredentialsInput{APIToken: &token}); err != nil {
		t.Fatal(err)
	}
	keyV2 := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	check(token, true, keyV2)
	manual := false
	if _, err := store.UpdateCredentials(ctx, created.ID, UpdateCredentialsInput{R2FromAPIToken: &manual}); err != nil {
		t.Fatal(err)
	}
	check(token, false, keyV2)
	token = "token-v3"
	if _, err := store.UpdateCredentials(ctx, created.ID, UpdateCredentialsInput{APIToken: &token}); err != nil {
		t.Fatal(err)
	}
	check(token, false, keyV2)
	auto := true
	if _, err := store.UpdateCredentials(ctx, created.ID, UpdateCredentialsInput{R2FromAPIToken: &auto}); err != nil {
		t.Fatal(err)
	}
	check(token, true, fmt.Sprintf("%x", sha256.Sum256([]byte(token))))
	access, key := "independent-access", "independent-secret"
	if _, err := store.UpdateCredentials(ctx, created.ID, UpdateCredentialsInput{R2AccessKeyID: &access, R2SecretAccessKey: &key}); err != nil {
		t.Fatal(err)
	}
	check(token, false, key)
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM encrypted_secrets").Scan(&count); err != nil || count != 3 {
		t.Fatalf("old secrets leaked: count=%d error=%v", count, err)
	}
}

func TestDerivationFailurePreservesCredentialsAndCreatesNoAccount(t *testing.T) {
	t.Parallel()
	store := tokenTestStore(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401); fmt.Fprint(w, `{"success":false}`) })
	ctx := context.Background()
	if _, err := store.Create(ctx, CreateInput{Name: "rejected", CloudflareAccountID: "account", APIToken: "invalid-token", R2FromAPIToken: true}); err == nil {
		t.Fatal("invalid token accepted")
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM encrypted_secrets").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed create left secrets")
	}
	account, err := store.Create(ctx, CreateInput{Name: "manual", CloudflareAccountID: "account", APIToken: "original", R2AccessKeyID: "access", R2SecretAccessKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	auto, token := true, "rejected"
	if _, err := store.UpdateCredentials(ctx, account.ID, UpdateCredentialsInput{R2FromAPIToken: &auto, APIToken: &token}); err == nil {
		t.Fatal("invalid token update accepted")
	}
	got, err := store.Get(ctx, account.ID, true)
	if err != nil || got.APIToken != "original" || got.R2AccessKeyID != "access" || got.R2SecretAccessKey != "key" || got.R2FromAPIToken {
		t.Fatal("failed derivation changed credentials")
	}
}

func TestDerivationRejectsConcurrentCredentialChange(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	store := tokenTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
			activeToken(w, r)
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	account, err := store.Create(ctx, CreateInput{Name: "race", CloudflareAccountID: "account", APIToken: "original"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		auto := true
		_, err := store.UpdateCredentials(ctx, account.ID, UpdateCredentialsInput{R2FromAPIToken: &auto})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("derivation did not start")
	}
	token := "concurrent-token"
	_, updateErr := store.UpdateCredentials(ctx, account.ID, UpdateCredentialsInput{APIToken: &token})
	close(release)
	if updateErr != nil {
		t.Fatal(updateErr)
	}
	if err := <-done; !errors.Is(err, ErrCredentialsChanged) {
		t.Fatalf("stale derivation result: %v", err)
	}
	got, err := store.Get(ctx, account.ID, true)
	if err != nil || got.APIToken != token || got.R2FromAPIToken || got.HasR2Credentials {
		t.Fatal("stale derivation overwrote new credentials")
	}
}

func TestAutomaticCredentialWriteRollsBackTogether(t *testing.T) {
	t.Parallel()
	store := tokenTestStore(t, activeToken)
	ctx := context.Background()
	account, err := store.Create(ctx, CreateInput{Name: "atomic", CloudflareAccountID: "account", APIToken: "old-token", R2FromAPIToken: true})
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(ctx, account.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_credentials BEFORE UPDATE ON accounts BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	token := "replacement-token"
	if _, err := store.UpdateCredentials(ctx, account.ID, UpdateCredentialsInput{APIToken: &token}); err == nil {
		t.Fatal("injected transaction failure ignored")
	}
	after, err := store.Get(ctx, account.ID, true)
	if err != nil || after.apiTokenSecretID != before.apiTokenSecretID || after.r2AccessSecretID != before.r2AccessSecretID || after.r2SecretSecretID != before.r2SecretSecretID || after.APIToken != before.APIToken || after.R2SecretAccessKey != before.R2SecretAccessKey {
		t.Fatal("partial credential write escaped rollback")
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM encrypted_secrets").Scan(&count); err != nil || count != 3 {
		t.Fatal("rollback left new secrets")
	}
}
