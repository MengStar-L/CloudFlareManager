package accounts

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestStoreEncryptsCredentialsAndRoundTripsAccount(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{7}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, secret.NewRepository(db, cipher))

	created, err := store.Create(context.Background(), CreateInput{
		Name: "primary", CloudflareAccountID: "cf-account", APIToken: "api-secret",
		R2AccessKeyID: "r2-key", R2SecretAccessKey: "r2-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), created.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIToken != "api-secret" || got.R2SecretAccessKey != "r2-secret" {
		t.Fatalf("credentials did not round trip: %#v", got)
	}

	var plaintextCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM encrypted_secrets WHERE CAST(ciphertext AS TEXT) LIKE '%api-secret%'").Scan(&plaintextCount); err != nil {
		t.Fatal(err)
	}
	if plaintextCount != 0 {
		t.Fatal("plaintext token found in database")
	}
}

func TestUpdateCredentialsReplacesSecretsWithoutChangingAccountIdentity(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{8}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, secret.NewRepository(db, cipher))
	ctx := context.Background()
	created, err := store.Create(ctx, CreateInput{
		Name: "primary", CloudflareAccountID: "cf-account", APIToken: "old-api-token",
		R2AccessKeyID: "old-access-key", R2SecretAccessKey: "old-secret-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCapabilities(ctx, created.ID, []Capability{{Name: "r2", Available: true}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetHealth(ctx, created.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(ctx, created.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	oldSecretIDs := []string{before.apiTokenSecretID, before.r2AccessSecretID.String, before.r2SecretSecretID.String}

	newAPIToken, newAccessKey, newSecretKey := "new-api-token", "new-access-key", "new-secret-key"
	updated, err := store.UpdateCredentials(ctx, created.ID, UpdateCredentialsInput{
		APIToken: &newAPIToken, R2AccessKeyID: &newAccessKey, R2SecretAccessKey: &newSecretKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID || updated.CloudflareAccountID != created.CloudflareAccountID ||
		updated.Name != created.Name || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("account identity changed during credential update: before=%#v after=%#v", created, updated)
	}
	if !updated.HasR2Credentials || updated.HealthStatus != "unknown" || len(updated.Capabilities) != 0 {
		t.Fatalf("updated account state = %#v", updated)
	}
	withSecrets, err := store.Get(ctx, created.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if withSecrets.APIToken != newAPIToken || withSecrets.R2AccessKeyID != newAccessKey ||
		withSecrets.R2SecretAccessKey != newSecretKey {
		t.Fatalf("updated credentials did not round trip: %#v", withSecrets)
	}
	var oldSecretCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM encrypted_secrets WHERE id IN (?, ?, ?)`,
		oldSecretIDs[0], oldSecretIDs[1], oldSecretIDs[2]).Scan(&oldSecretCount); err != nil {
		t.Fatal(err)
	}
	if oldSecretCount != 0 {
		t.Fatalf("old encrypted secrets retained after update: %d", oldSecretCount)
	}

	apiOnly := "api-token-rotated-again"
	if _, err := store.UpdateCredentials(ctx, created.ID, UpdateCredentialsInput{APIToken: &apiOnly}); err != nil {
		t.Fatal(err)
	}
	withSecrets, err = store.Get(ctx, created.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if withSecrets.APIToken != apiOnly || withSecrets.R2AccessKeyID != newAccessKey ||
		withSecrets.R2SecretAccessKey != newSecretKey {
		t.Fatalf("API-only update changed R2 credentials: %#v", withSecrets)
	}

	cleared, err := store.UpdateCredentials(ctx, created.ID, UpdateCredentialsInput{ClearR2Credentials: true})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.HasR2Credentials {
		t.Fatal("account still reports R2 credentials after removal")
	}
	withSecrets, err = store.Get(ctx, created.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if withSecrets.APIToken != apiOnly || withSecrets.R2AccessKeyID != "" || withSecrets.R2SecretAccessKey != "" {
		t.Fatalf("clearing R2 credentials changed the wrong values: %#v", withSecrets)
	}
	var remainingSecretCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM encrypted_secrets WHERE scope = ?`, "account:"+created.ID).Scan(&remainingSecretCount); err != nil {
		t.Fatal(err)
	}
	if remainingSecretCount != 1 {
		t.Fatalf("encrypted secret count after clearing R2 credentials = %d, want 1", remainingSecretCount)
	}
	if err := store.SetCapabilities(ctx, created.ID, []Capability{{Name: "api_token", Available: true}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetHealth(ctx, created.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}
	restoredAccess, restoredSecret := "restored-access-key", "restored-secret-key"
	restored, err := store.UpdateCredentials(ctx, created.ID, UpdateCredentialsInput{
		R2AccessKeyID: &restoredAccess, R2SecretAccessKey: &restoredSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !restored.HasR2Credentials || restored.HealthStatus != "healthy" || len(restored.Capabilities) != 1 {
		t.Fatalf("R2-only update changed API-derived account state: %#v", restored)
	}
	withSecrets, err = store.Get(ctx, created.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if withSecrets.APIToken != apiOnly || withSecrets.R2AccessKeyID != restoredAccess ||
		withSecrets.R2SecretAccessKey != restoredSecret {
		t.Fatalf("restoring R2 credentials changed the wrong values: %#v", withSecrets)
	}
}

func TestUpdateCredentialsRejectsPartialR2ReplacementWithoutChangingSecrets(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{9}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, secret.NewRepository(db, cipher))
	ctx := context.Background()
	created, err := store.Create(ctx, CreateInput{
		Name: "primary", CloudflareAccountID: "cf-account", APIToken: "api-token",
		R2AccessKeyID: "access-key", R2SecretAccessKey: "secret-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	accessKey, secretKey := "replacement-access", "replacement-secret"
	tests := []UpdateCredentialsInput{
		{},
		{R2AccessKeyID: &accessKey},
		{R2SecretAccessKey: &secretKey},
		{R2AccessKeyID: &accessKey, R2SecretAccessKey: &secretKey, ClearR2Credentials: true},
	}
	for _, input := range tests {
		if _, err := store.UpdateCredentials(ctx, created.ID, input); err == nil {
			t.Fatalf("UpdateCredentials(%#v) succeeded, want validation error", input)
		}
	}
	got, err := store.Get(ctx, created.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIToken != "api-token" || got.R2AccessKeyID != "access-key" || got.R2SecretAccessKey != "secret-key" {
		t.Fatalf("invalid update changed credentials: %#v", got)
	}
	var secretCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM encrypted_secrets WHERE scope = ?`, "account:"+created.ID).Scan(&secretCount); err != nil {
		t.Fatal(err)
	}
	if secretCount != 3 {
		t.Fatalf("encrypted secret count after rejected updates = %d, want 3", secretCount)
	}
}

func TestUpdateCredentialsRollsBackSecretReplacementWhenAccountUpdateFails(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{12}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, secret.NewRepository(db, cipher))
	ctx := context.Background()
	created, err := store.Create(ctx, CreateInput{
		Name: "primary", CloudflareAccountID: "cf-account", APIToken: "old-api-token",
		R2AccessKeyID: "old-access-key", R2SecretAccessKey: "old-secret-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_account_credential_update
		BEFORE UPDATE ON accounts BEGIN SELECT RAISE(ABORT, 'forced account update failure'); END`); err != nil {
		t.Fatal(err)
	}

	newToken, newAccess, newSecret := "new-api-token", "new-access-key", "new-secret-key"
	if _, err := store.UpdateCredentials(ctx, created.ID, UpdateCredentialsInput{
		APIToken: &newToken, R2AccessKeyID: &newAccess, R2SecretAccessKey: &newSecret,
	}); err == nil {
		t.Fatal("UpdateCredentials succeeded despite rejecting account update")
	}
	got, err := store.Get(ctx, created.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIToken != "old-api-token" || got.R2AccessKeyID != "old-access-key" || got.R2SecretAccessKey != "old-secret-key" {
		t.Fatalf("failed update changed stored credentials: %#v", got)
	}
	var secretCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM encrypted_secrets WHERE scope = ?`, "account:"+created.ID).Scan(&secretCount); err != nil {
		t.Fatal(err)
	}
	if secretCount != 3 {
		t.Fatalf("failed update left replacement secrets behind: got %d secrets, want 3", secretCount)
	}
}

func TestDeleteReportsRegisteredBucketsAndActiveDeletionJobs(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{13}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, secret.NewRepository(db, cipher))
	ctx := context.Background()
	account, err := store.Create(ctx, CreateInput{
		Name: "primary", CloudflareAccountID: "cf-account", APIToken: "api-token",
		R2AccessKeyID: "access-key", R2SecretAccessKey: "secret-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"archive", "assets", "backups", "media", "private", "uploads"} {
		if _, err := db.Exec(`INSERT INTO r2_physical_buckets(id, account_id, bucket_name, created_at, updated_at)
			VALUES(?, ?, ?, 1, 1)`, "bucket-"+string(rune('a'+i)), account.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO jobs(id, type, status, payload_json, resource_key, created_at, updated_at)
		VALUES('delete-job', 'r2.bucket.delete-remote', 'running', '{}', ?, 1, 1)`,
		account.ID+"/default/uploads"); err != nil {
		t.Fatal(err)
	}

	err = store.Delete(ctx, account.ID)
	if !errors.Is(err, ErrAccountInUse) {
		t.Fatalf("Delete error = %v, want ErrAccountInUse", err)
	}
	var inUse *AccountInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("Delete error type = %T, want *AccountInUseError", err)
	}
	if len(inUse.Blockers) != 2 {
		t.Fatalf("blockers = %#v, want bucket and deletion-job blockers", inUse.Blockers)
	}
	buckets := inUse.Blockers[0]
	if buckets.Kind != DeletionBlockerR2Bucket || buckets.Count != 6 || len(buckets.Items) != deletionBlockerPreviewLimit || !buckets.Truncated {
		t.Fatalf("bucket blocker = %#v", buckets)
	}
	if buckets.Items[0].Name != "archive" || buckets.Items[4].Name != "private" {
		t.Fatalf("bucket preview is not stable and sorted: %#v", buckets.Items)
	}
	deletionJobs := inUse.Blockers[1]
	if deletionJobs.Kind != DeletionBlockerR2DeletionJob || deletionJobs.Count != 1 || deletionJobs.Truncated ||
		len(deletionJobs.Items) != 1 || deletionJobs.Items[0].Name != "uploads" || deletionJobs.Items[0].Status != "running" {
		t.Fatalf("deletion-job blocker = %#v", deletionJobs)
	}
	if _, err := store.Get(ctx, account.ID, true); err != nil {
		t.Fatalf("blocked deletion changed account: %v", err)
	}
	var secretCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM encrypted_secrets WHERE scope = ?", "account:"+account.ID).Scan(&secretCount); err != nil {
		t.Fatal(err)
	}
	if secretCount != 3 {
		t.Fatalf("blocked deletion retained %d secrets, want 3", secretCount)
	}
}

func TestDeleteRemovesAccountOwnedAIHistoryAndSecrets(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{14}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, secret.NewRepository(db, cipher))
	ctx := context.Background()
	account, err := store.Create(ctx, CreateInput{
		Name: "history-only", CloudflareAccountID: "cf-history", APIToken: "api-token",
		R2AccessKeyID: "access-key", R2SecretAccessKey: "secret-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ai_request_logs(id, account_id, model, status_code, created_at)
		VALUES('request-log', ?, '@cf/test', 200, 1)`, account.ID); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, account.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get deleted account error = %v, want ErrNotFound", err)
	}
	for name, query := range map[string]string{
		"AI request logs":   "SELECT COUNT(*) FROM ai_request_logs WHERE account_id = ?",
		"encrypted secrets": "SELECT COUNT(*) FROM encrypted_secrets WHERE scope = 'account:' || ?",
	} {
		var count int
		if err := db.QueryRow(query, account.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s after account deletion = %d, want 0", name, count)
		}
	}
}

func TestDeleteRollsBackAccountAndHistoryWhenSecretCleanupFails(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{15}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, secret.NewRepository(db, cipher))
	ctx := context.Background()
	account, err := store.Create(ctx, CreateInput{Name: "primary", CloudflareAccountID: "cf-account", APIToken: "api-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ai_request_logs(id, account_id, model, status_code, created_at)
		VALUES('request-log', ?, '@cf/test', 200, 1)`, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_account_secret_delete BEFORE DELETE ON encrypted_secrets
		BEGIN SELECT RAISE(ABORT, 'forced secret cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}

	err = store.Delete(ctx, account.ID)
	if err == nil || errors.Is(err, ErrAccountInUse) {
		t.Fatalf("Delete error = %v, want non-conflict cleanup failure", err)
	}
	if _, err := store.Get(ctx, account.ID, true); err != nil {
		t.Fatalf("account was not rolled back: %v", err)
	}
	var logCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM ai_request_logs WHERE account_id = ?", account.ID).Scan(&logCount); err != nil {
		t.Fatal(err)
	}
	if logCount != 1 {
		t.Fatalf("AI history after rollback = %d, want 1", logCount)
	}
}
