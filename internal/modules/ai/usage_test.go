package ai

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/credentials"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
	"github.com/google/uuid"
)

func TestParseUsageDate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 18, 0, 0, 0, time.FixedZone("test", 8*60*60))
	for _, test := range []struct {
		raw  string
		want string
		ok   bool
	}{
		{raw: "", want: "2026-07-29", ok: true},
		{raw: "2028-02-29", want: "2028-02-29", ok: true},
		{raw: "2026-02-29"},
		{raw: "2026-07-29T00:00:00Z"},
		{raw: " 2026-07-29"},
	} {
		day, err := ParseUsageDate(test.raw, now)
		if (err == nil) != test.ok {
			t.Fatalf("ParseUsageDate(%q) error = %v", test.raw, err)
		}
		if test.ok && (day.Format("2006-01-02") != test.want || day.Location() != time.UTC) {
			t.Fatalf("ParseUsageDate(%q) = %v", test.raw, day)
		}
	}
}

func TestDailyUsageGroupsAccountsAndCredentials(t *testing.T) {
	t.Parallel()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{19}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	secrets := secret.NewRepository(db, cipher)
	accountStore := accounts.NewStore(db, secrets)
	credentialStore := credentials.NewStore(db, secrets)
	first := createUsageAccount(t, accountStore, "Alpha")
	second := createUsageAccount(t, accountStore, "Beta")
	active, err := credentialStore.Create(context.Background(), credentials.CreateInput{
		Kind: credentials.KindAI, Name: "Sub2API", Scopes: []string{"ai:invoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := credentialStore.Create(context.Background(), credentials.CreateInput{
		Kind: credentials.KindAI, Name: "Revoked", Scopes: []string{"ai:invoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := credentialStore.Revoke(context.Background(), revoked.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := credentialStore.Create(context.Background(), credentials.CreateInput{
		Kind: credentials.KindAI, Name: "Deleted", Scopes: []string{"ai:invoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := credentialStore.Revoke(context.Background(), deleted.ID); err != nil {
		t.Fatal(err)
	}
	if err := credentialStore.Delete(context.Background(), deleted.ID); err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO ai_usage_daily(account_id, usage_date, estimated_neurons, requests, errors)
		VALUES(?, ?, 600, 5, 1)`, first.ID, day.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	insertUsageLog(t, db, first.ID, active.ID, 200, 200, day.Add(time.Hour), "")
	insertUsageLog(t, db, first.ID, revoked.ID, 100, 200, day.Add(2*time.Hour), "")
	insertUsageLog(t, db, first.ID, deleted.ID, 50, 200, day.Add(3*time.Hour), "")
	insertUsageLog(t, db, first.ID, "", 150, 200, day.Add(4*time.Hour), "")
	insertUsageLog(t, db, first.ID, "admin", 100, 500, day.Add(5*time.Hour), "upstream_error")
	insertUsageLog(t, db, first.ID, active.ID, 999, 200, day.Add(-time.Nanosecond), "")

	report, err := (UsageService{DB: db, Accounts: accountStore}).Daily(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if report.Date != "2026-07-29" || report.Timezone != "UTC" || report.DailyLimit != 10_000 || !report.Estimated {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Accounts) != 2 || report.Accounts[0].AccountID != first.ID || report.Accounts[1].AccountID != second.ID {
		t.Fatalf("accounts = %#v", report.Accounts)
	}
	if report.Accounts[0].EstimatedUsed != 600 || report.Accounts[0].EstimatedRemaining != 9_400 || report.Accounts[0].Requests != 5 || report.Accounts[0].Errors != 1 {
		t.Fatalf("first account = %#v", report.Accounts[0])
	}
	if report.Accounts[1].EstimatedUsed != 0 || report.Accounts[1].EstimatedRemaining != 10_000 {
		t.Fatalf("second account = %#v", report.Accounts[1])
	}
	byStatus := make(map[string]CredentialUsage)
	for _, item := range report.Accounts[0].Credentials {
		byStatus[item.Status] = item
	}
	if byStatus["active"].EstimatedUsed != 200 || byStatus["revoked"].EstimatedUsed != 100 ||
		byStatus["deleted"].EstimatedUsed != 50 || byStatus["unattributed"].EstimatedUsed != 250 {
		t.Fatalf("credential usage = %#v", report.Accounts[0].Credentials)
	}
}

func createUsageAccount(t *testing.T, store *accounts.Store, name string) accounts.Account {
	t.Helper()
	account, err := store.Create(context.Background(), accounts.CreateInput{
		Name: name, CloudflareAccountID: "cf-" + name, APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCapabilities(context.Background(), account.ID, []accounts.Capability{{Name: "ai", Available: true}}); err != nil {
		t.Fatal(err)
	}
	return account
}

func insertUsageLog(t *testing.T, db execer, accountID, credentialID string, neurons float64, status int, created time.Time, errorClass string) {
	t.Helper()
	var credential any
	if credentialID != "" {
		credential = credentialID
	}
	if _, err := db.Exec(`INSERT INTO ai_request_logs(id, protocol_credential_id, account_id, model, status_code,
		input_tokens, output_tokens, estimated_neurons, duration_ms, error_class, created_at, neuron_estimation_source)
		VALUES(?, ?, ?, '@cf/test', ?, 1, 1, ?, 1, ?, ?, 'test')`,
		uuid.NewString(), credential, accountID, status, neurons, errorClass, created.UnixNano()); err != nil {
		t.Fatal(err)
	}
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}
