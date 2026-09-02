package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenAppliesMigrationsAndPragmas(t *testing.T) {
	t.Parallel()

	db, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var journal string
	if err := db.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Fatalf("journal mode = %q", journal)
	}

	var count int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("expected applied migration")
	}

	var paidModelsTable int
	if err := db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'ai_paid_models'`).Scan(&paidModelsTable); err != nil {
		t.Fatal(err)
	}
	if paidModelsTable != 1 {
		t.Fatal("ai_paid_models migration was not applied")
	}
	rows, err := db.QueryContext(context.Background(), "PRAGMA table_info(ai_request_logs)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	foundSource := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		foundSource = foundSource || name == "neuron_estimation_source"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !foundSource {
		t.Fatal("neuron_estimation_source column was not applied")
	}
}

func TestTransactionalPlacementMigrationBackfillsLegacyUsageAndMultipart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_initial.sql", "002_protocol_credentials.sql", "003_r2_multipart_parts.sql", "004_r2_scan_findings.sql", "005_ai_model_policy_usage.sql"} {
		script, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.Exec(string(script)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := legacy.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?, 1)`, name); err != nil {
			t.Fatal(err)
		}
	}
	statements := []string{
		`INSERT INTO encrypted_secrets(id, scope, kind, ciphertext, created_at, updated_at) VALUES('secret', 'test', 'api', X'00', 1, 1)`,
		`INSERT INTO accounts(id, name, cloudflare_account_id, api_token_secret_id, created_at, updated_at) VALUES('account', 'account', 'cloudflare', 'secret', 1, 1)`,
		`INSERT INTO r2_physical_buckets(id, account_id, bucket_name, storage_bytes, health_status, created_at, updated_at) VALUES('bucket', 'account', 'bucket', 2, 'healthy', 1, 1)`,
		`INSERT INTO r2_objects(object_key, object_id, physical_bucket_id, physical_key, state, size, last_modified, created_at, updated_at) VALUES('old.bin', 'object', 'bucket', 'old.bin', 'committed', 5, 1, 1, 1)`,
		`INSERT INTO r2_multipart_uploads(id, object_key, object_id, physical_bucket_id, upstream_upload_id, metadata_json, status, created_at, updated_at) VALUES('upload', 'new.bin', 'new-object', 'bucket', 'upstream', '{"content_type":"application/octet-stream","metadata":{"source":"legacy"}}', 'active', 1, 1)`,
		`INSERT INTO r2_multipart_parts(upload_id, part_number, etag, size, updated_at) VALUES('upload', 1, 'etag', 7, 1)`,
	}
	for _, statement := range statements {
		if _, err := legacy.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var storage, reserved, checked int64
	if err := db.QueryRow(`SELECT storage_bytes, reserved_storage_bytes, usage_checked_at
		FROM r2_physical_buckets WHERE id = 'bucket'`).Scan(&storage, &reserved, &checked); err != nil {
		t.Fatal(err)
	}
	if storage != 5 || reserved != 7 || checked != 0 {
		t.Fatalf("bucket storage=%d reserved=%d checked=%d", storage, reserved, checked)
	}
	var intentID, operation, metadata string
	if err := db.QueryRow(`SELECT write_intent_id FROM r2_multipart_uploads WHERE id = 'upload'`).Scan(&intentID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT operation, metadata_json FROM r2_write_intents WHERE id = ?`, intentID).Scan(&operation, &metadata); err != nil {
		t.Fatal(err)
	}
	if operation != "legacy-multipart" || metadata != `{"source":"legacy"}` {
		t.Fatalf("operation=%q metadata=%q", operation, metadata)
	}
}

func TestRemoteBucketDeletionMigrationAddsJobIdentityAndBucketLifecycle(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"001_initial.sql",
		"002_protocol_credentials.sql",
		"003_r2_multipart_parts.sql",
		"004_r2_scan_findings.sql",
		"005_ai_model_policy_usage.sql",
		"006_r2_transactional_placement.sql",
	} {
		script, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.Exec(string(script)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := legacy.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?, 1)`, name); err != nil {
			t.Fatal(err)
		}
	}
	statements := []string{
		`INSERT INTO encrypted_secrets(id, scope, kind, ciphertext, created_at, updated_at) VALUES('secret', 'test', 'api', X'00', 1, 1)`,
		`INSERT INTO accounts(id, name, cloudflare_account_id, api_token_secret_id, created_at, updated_at) VALUES('account', 'account', 'cloudflare', 'secret', 1, 1)`,
		`INSERT INTO r2_physical_buckets(id, account_id, bucket_name, health_status, created_at, updated_at) VALUES('bucket', 'account', 'bucket', 'healthy', 1, 1)`,
		`INSERT INTO jobs(id, type, status, payload_json, progress, attempts, max_attempts, error, created_at, updated_at) VALUES('done', 'test', 'succeeded', '{}', 1, 1, 1, '', 1, 1)`,
		`INSERT INTO jobs(id, type, status, payload_json, progress, attempts, max_attempts, error, created_at, updated_at) VALUES('failed', 'test', 'failed', '{}', 0, 1, 1, 'failed', 1, 1)`,
	}
	for _, statement := range statements {
		if _, err := legacy.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var lifecycle, deletionJobID string
	if err := db.QueryRow(`SELECT lifecycle_state, COALESCE(deletion_job_id, '')
		FROM r2_physical_buckets WHERE id = 'bucket'`).Scan(&lifecycle, &deletionJobID); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "active" || deletionJobID != "" {
		t.Fatalf("lifecycle=%q deletion_job_id=%q", lifecycle, deletionJobID)
	}

	insertJob := `INSERT INTO jobs(
		id, type, status, payload_json, max_attempts, resource_key, created_at, updated_at)
		VALUES(?, 'r2.bucket.delete-remote', ?, '{}', 4, 'account/default/bucket', 2, 2)`
	if _, err := db.Exec(insertJob, "active-1", "pending"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insertJob, "active-2", "running"); err == nil {
		t.Fatal("expected duplicate active resource job to fail")
	}
	if _, err := db.Exec(insertJob, "terminal", "failed"); err != nil {
		t.Fatalf("terminal job with the same resource key: %v", err)
	}
	if _, err := db.Exec(`UPDATE r2_physical_buckets SET lifecycle_state = 'unknown' WHERE id = 'bucket'`); err == nil {
		t.Fatal("expected invalid bucket lifecycle to fail")
	}
}
