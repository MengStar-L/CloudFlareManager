package r2

import (
	"errors"
	"testing"
	"time"
)

func insertLifecycleTestJob(t *testing.T, store *Store, id string) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := store.db.Exec(`INSERT INTO jobs(
		id, type, status, payload_json, progress, attempts, max_attempts, error, created_at, updated_at)
		VALUES(?, 'r2.bucket.delete-remote', 'running', '{}', 0, 1, 4, '', ?, ?)`, id, now, now); err != nil {
		t.Fatal(err)
	}
}

func TestBeginBucketDeletionFencesAndRetriesByParent(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, AccountStorageBytes: 1000, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "lifecycle")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	if bucket.LifecycleState != BucketActive {
		t.Fatalf("new bucket lifecycle = %q", bucket.LifecycleState)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE r2_physical_buckets SET writable = 0 WHERE id = ?`, bucket.ID); err != nil {
		t.Fatal(err)
	}
	insertLifecycleTestJob(t, store, "first-job")
	insertLifecycleTestJob(t, store, "wrong-parent")
	insertLifecycleTestJob(t, store, "retry-job")
	if _, err := store.BeginBucketDeletion(ctx, bucket.ID, "first-job", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureBucketActive(ctx, bucket.ID); !errors.Is(err, ErrBucketDeleting) {
		t.Fatalf("EnsureBucketActive error = %v", err)
	}
	if err := store.MarkBucketDeletionFailed(ctx, bucket.ID, "first-job"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET status = 'failed' WHERE id = 'first-job'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginBucketDeletion(ctx, bucket.ID, "retry-job", "wrong-parent"); !errors.Is(err, ErrBucketDeleting) {
		t.Fatalf("wrong-parent retry error = %v", err)
	}
	got, err := store.BeginBucketDeletion(ctx, bucket.ID, "retry-job", "first-job")
	if err != nil {
		t.Fatal(err)
	}
	if got.LifecycleState != BucketDeleting || got.DeletionJobID != "retry-job" || got.Writable {
		t.Fatalf("retry bucket = %#v", got)
	}
	if err := store.RestoreBucketActive(ctx, bucket.ID, "retry-job"); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetBucket(ctx, bucket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LifecycleState != BucketActive || got.DeletionJobID != "" || got.Writable {
		t.Fatalf("restored bucket = %#v", got)
	}
}

func TestBeginBucketDeletionRecoversFenceOwnedByFailedJob(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, AccountStorageBytes: 1000, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "stuck-fence")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	insertLifecycleTestJob(t, store, "first-job")
	insertLifecycleTestJob(t, store, "retry-job")
	if _, err := store.BeginBucketDeletion(ctx, bucket.ID, "first-job", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireBucketDeletionMaintenance(ctx, bucket.ID, "first-job"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginBucketDeletion(ctx, bucket.ID, "retry-job", "first-job"); !errors.Is(err, ErrBucketDeleting) {
		t.Fatalf("running parent takeover error = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET status = 'failed' WHERE id = 'first-job'`); err != nil {
		t.Fatal(err)
	}
	got, err := store.BeginBucketDeletion(ctx, bucket.ID, "retry-job", "first-job")
	if err != nil {
		t.Fatal(err)
	}
	if got.LifecycleState != BucketDeleting || got.DeletionJobID != "retry-job" {
		t.Fatalf("recovered bucket = %#v", got)
	}
	var operation string
	if err := store.db.QueryRowContext(ctx, `SELECT operation FROM r2_bucket_maintenance_locks
		WHERE physical_bucket_id = ?`, bucket.ID).Scan(&operation); err != nil {
		t.Fatal(err)
	}
	if operation != "delete:retry-job" {
		t.Fatalf("transferred maintenance lock = %q", operation)
	}
}

func TestBeginBucketDeletionRejectsUnrelatedLockDuringRecovery(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"scan", "delete:other-job"} {
		operation := operation
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, AccountStorageBytes: 1000, ClassA: 100, ClassB: 100})
			account := createIntentAccount(t, accountStore, ctx, "foreign-lock-"+operation)
			bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
			if err != nil {
				t.Fatal(err)
			}
			insertLifecycleTestJob(t, store, "first-job")
			insertLifecycleTestJob(t, store, "retry-job")
			if _, err := store.BeginBucketDeletion(ctx, bucket.ID, "first-job", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, `INSERT INTO r2_bucket_maintenance_locks(
				physical_bucket_id, operation, created_at) VALUES(?, ?, ?)`, bucket.ID, operation, time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET status = 'failed' WHERE id = 'first-job'`); err != nil {
				t.Fatal(err)
			}
			if _, err := store.BeginBucketDeletion(ctx, bucket.ID, "retry-job", "first-job"); !errors.Is(err, ErrBucketBusy) {
				t.Fatalf("recovery with %q lock error = %v", operation, err)
			}
			got, err := store.GetBucket(ctx, bucket.ID)
			if err != nil || got.DeletionJobID != "first-job" {
				t.Fatalf("bucket after rejected recovery = %#v, %v", got, err)
			}
		})
	}
}

func TestDeletionBlockingActivityIncludesPreviousObjectBucket(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, AccountStorageBytes: 1000, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "blocking")
	source, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "source"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "target"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO r2_objects(
		object_key, object_id, physical_bucket_id, physical_key, state, size, etag,
		content_type, metadata_json, last_modified, error, created_at, updated_at)
		VALUES('file.bin', 'old-object', ?, 'file.bin', 'committed', 1, 'etag', '', '{}', ?, '', ?, ?)`,
		source.ID, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO r2_write_intents(
		id, object_key, target_bucket_id, previous_object_id, reserved_bytes, declared_size,
		actual_size, content_type, metadata_json, state, operation, upstream_upload_id, etag,
		internal_multipart, created_at, updated_at)
		VALUES('overwrite', 'file.bin', ?, 'old-object', 1, 1, 0, '', '{}', 'reserved',
		'put', '', '', 0, ?, ?)`, target.ID, now, now); err != nil {
		t.Fatal(err)
	}
	active, err := store.HasDeletionBlockingActivity(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("previous object mapping must block source bucket deletion")
	}
}

func TestFinalizeDeletedBucketRemovesReferencesAtomically(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, AccountStorageBytes: 1000, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "finalize")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	insertLifecycleTestJob(t, store, "delete-job")
	now := time.Now()
	nano := now.UnixNano()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO r2_objects(object_key, object_id, physical_bucket_id, physical_key, state,
			size, etag, content_type, metadata_json, last_modified, error, created_at, updated_at)
			VALUES('file.bin', 'object', ?, 'file.bin', 'committed', 1, 'etag', '', '{}', ?, '', ?, ?)`, []any{bucket.ID, nano, nano, nano}},
		{`INSERT INTO r2_write_intents(id, object_key, target_bucket_id, previous_object_id,
			reserved_bytes, declared_size, actual_size, content_type, metadata_json, state, operation,
			upstream_upload_id, etag, internal_multipart, created_at, updated_at)
			VALUES('intent', 'upload.bin', ?, NULL, 0, 0, 0, '', '{}', 'uploading', 'put', 'upstream', '', 1, ?, ?)`, []any{bucket.ID, nano, nano}},
		{`INSERT INTO r2_multipart_uploads(id, object_key, object_id, physical_bucket_id,
			upstream_upload_id, metadata_json, status, created_at, updated_at, write_intent_id)
			VALUES('upload', 'upload.bin', 'upload-object', ?, 'upstream', '{}', 'active', ?, ?, 'intent')`, []any{bucket.ID, nano, nano}},
		{`INSERT INTO r2_multipart_parts(upload_id, part_number, etag, size, updated_at)
			VALUES('upload', 1, 'part-etag', 1, ?)`, []any{nano}},
		{`INSERT INTO r2_multipart_part_reservations(upload_id, part_number, previous_size,
			requested_size, created_at, updated_at) VALUES('upload', 1, 0, 1, ?, ?)`, []any{nano, nano}},
		{`INSERT INTO r2_physical_cleanups(id, object_key, physical_bucket_id, physical_key,
			expected_etag, size, status, error, created_at, updated_at)
			VALUES('cleanup', 'stale.bin', ?, 'stale.bin', '', 1, 'pending', '', ?, ?)`, []any{bucket.ID, nano, nano}},
		{`INSERT INTO r2_scan_findings(id, physical_bucket_id, physical_key, kind, detail, found_at)
			VALUES('finding', ?, 'orphan.bin', 'orphan', '', ?)`, []any{bucket.ID, now.Unix()}},
		{`INSERT INTO r2_placement_rules(id, priority, target_bucket_id, created_at, updated_at)
			VALUES('rule', 1, ?, ?, ?)`, []any{bucket.ID, now.Unix(), now.Unix()}},
		{`INSERT INTO webdav_locks(token, object_key, owner, depth, expires_at, created_at)
			VALUES('expired-lock', 'old', '', '0', ?, ?)`, []any{now.Add(-time.Minute).Unix(), now.Unix()}},
		{`INSERT INTO webdav_locks(token, object_key, owner, depth, expires_at, created_at)
			VALUES('active-lock', 'active', '', '0', ?, ?)`, []any{now.Add(time.Hour).Unix(), now.Unix()}},
	}
	for _, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.BeginBucketDeletion(ctx, bucket.ID, "delete-job", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeDeletedBucket(ctx, bucket.ID, "delete-job"); !errors.Is(err, ErrBucketBusy) {
		t.Fatalf("finalize with active writes error = %v", err)
	}
	for _, table := range []string{"r2_write_intents", "r2_multipart_uploads", "r2_objects"} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Fatalf("%s was removed before deletion settlement", table)
		}
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM r2_multipart_uploads WHERE physical_bucket_id = ?`, bucket.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM r2_write_intents WHERE target_bucket_id = ?`, bucket.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireBucketDeletionMaintenance(ctx, bucket.ID, "delete-job"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeDeletedBucket(ctx, bucket.ID, "delete-job"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBucket(ctx, bucket.ID); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("GetBucket after finalize error = %v", err)
	}
	for _, table := range []string{"r2_objects", "r2_write_intents", "r2_multipart_uploads", "r2_multipart_parts", "r2_multipart_part_reservations", "r2_physical_cleanups", "r2_scan_findings", "r2_placement_rules", "r2_bucket_maintenance_locks"} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows after finalize = %d", table, count)
		}
	}
	var activeLocks, expiredLocks int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webdav_locks WHERE token = 'active-lock'`).Scan(&activeLocks); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webdav_locks WHERE token = 'expired-lock'`).Scan(&expiredLocks); err != nil {
		t.Fatal(err)
	}
	if activeLocks != 1 || expiredLocks != 0 {
		t.Fatalf("active locks=%d expired locks=%d", activeLocks, expiredLocks)
	}
}

func TestFinalizeDeletedBucketRequiresMatchingMaintenanceLock(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, AccountStorageBytes: 1000, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "finalize-lock")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	insertLifecycleTestJob(t, store, "delete-job")
	if _, err := store.BeginBucketDeletion(ctx, bucket.ID, "delete-job", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeDeletedBucket(ctx, bucket.ID, "delete-job"); !errors.Is(err, ErrBucketBusy) {
		t.Fatalf("finalize without lock error = %v", err)
	}
	if _, err := store.GetBucket(ctx, bucket.ID); err != nil {
		t.Fatalf("bucket was removed without lock: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO r2_bucket_maintenance_locks(
		physical_bucket_id, operation, created_at) VALUES(?, 'delete:other-job', ?)`, bucket.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeDeletedBucket(ctx, bucket.ID, "delete-job"); !errors.Is(err, ErrBucketBusy) {
		t.Fatalf("finalize with another job's lock error = %v", err)
	}
	if _, err := store.GetBucket(ctx, bucket.ID); err != nil {
		t.Fatalf("bucket was removed with mismatched lock: %v", err)
	}
}

func TestDeletingBucketRejectsReadsWritesAndMaintenance(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, AccountStorageBytes: 1000, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "fenced")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishBucketScan(ctx, bucket.ID, 1, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO r2_objects(
		object_key, object_id, physical_bucket_id, physical_key, state, size, etag,
		content_type, metadata_json, last_modified, error, created_at, updated_at)
		VALUES('file.bin', 'object', ?, 'file.bin', 'committed', 1, 'etag', '', '{}', ?, '', ?, ?)`,
		bucket.ID, now, now, now); err != nil {
		t.Fatal(err)
	}
	insertLifecycleTestJob(t, store, "fence-job")
	if _, err := store.BeginBucketDeletion(ctx, bucket.ID, "fence-job", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "file.bin", Size: 2}}); !errors.Is(err, ErrBucketDeleting) {
		t.Fatalf("overwrite error = %v", err)
	}
	if _, _, err := store.BeginDeleteWrite(ctx, "file.bin"); !errors.Is(err, ErrBucketDeleting) {
		t.Fatalf("delete error = %v", err)
	}
	if err := store.AcquireBucketMaintenance(ctx, bucket.ID, "scan"); !errors.Is(err, ErrBucketDeleting) {
		t.Fatalf("maintenance error = %v", err)
	}
	service := Service{Index: store}
	if _, err := service.Stat(ctx, "file.bin"); !errors.Is(err, ErrBucketDeleting) {
		t.Fatalf("Stat error = %v", err)
	}
	list, err := service.List(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Objects) != 1 || list.Objects[0].Key != "file.bin" {
		t.Fatalf("list while deleting = %#v", list)
	}
}

func TestDeletingBucketRejectsMultipartProgressButAllowsAbort(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, AccountStorageBytes: 1000, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "multipart-fence")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishBucketScan(ctx, bucket.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	upload, err := store.BeginMultipart(ctx, ObjectInput{Key: "large.bin", Size: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateMultipart(ctx, upload.ID, "remote-upload"); err != nil {
		t.Fatal(err)
	}
	insertLifecycleTestJob(t, store, "multipart-delete-job")
	if _, err := store.BeginBucketDeletion(ctx, bucket.ID, "multipart-delete-job", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareMultipartPart(ctx, upload.ID, 1, 1); !errors.Is(err, ErrBucketDeleting) {
		t.Fatalf("PrepareMultipartPart error = %v", err)
	}
	if err := store.BeginCompleteMultipart(ctx, upload.ID); !errors.Is(err, ErrBucketDeleting) {
		t.Fatalf("BeginCompleteMultipart error = %v", err)
	}
	if err := store.AbortClientMultipart(ctx, upload.ID); err != nil {
		t.Fatalf("AbortClientMultipart error = %v", err)
	}
}

func TestSettleBucketForDeletionAbortsIdleMultipart(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, AccountStorageBytes: 1000, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "settle")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishBucketScan(ctx, bucket.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	backend := &memoryBackend{
		objects:  make(map[string][]byte),
		metadata: make(map[string]map[string]string),
		etags:    make(map[string]string),
	}
	service := Service{Index: store, Accounts: accountStore, Backend: backend}
	upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: "large.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := backend.uploads[upload.UpstreamID]; !ok {
		t.Fatal("remote multipart was not created")
	}
	insertLifecycleTestJob(t, store, "settle-job")
	if _, err := store.BeginBucketDeletion(ctx, bucket.ID, "settle-job", ""); err != nil {
		t.Fatal(err)
	}
	settled, err := service.SettleBucketForDeletion(ctx, bucket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !settled {
		t.Fatal("bucket should be settled")
	}
	if _, ok := backend.uploads[upload.UpstreamID]; ok {
		t.Fatal("remote multipart was not aborted")
	}
	if active, err := store.HasDeletionBlockingActivity(ctx, bucket.ID); err != nil || active {
		t.Fatalf("active=%v error=%v", active, err)
	}
}
