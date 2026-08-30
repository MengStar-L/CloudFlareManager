package r2

import (
	"errors"
	"strings"
	"testing"
)

func TestCrossBucketCleanupUsesETagFenceAndUpdatesUsageAfterConfirmation(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 200, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "primary")
	first, _ := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "first"})
	second, _ := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "second"})
	_ = store.FinishBucketScan(ctx, first.ID, 0, false)
	_ = store.FinishBucketScan(ctx, second.ID, 0, false)
	backend := &memoryBackend{objects: map[string][]byte{}}
	service := Service{Index: store, Accounts: accountStore, Backend: backend, TempDir: t.TempDir()}

	oldObject, err := service.Put(ctx, PutRequest{Key: "same.bin", Body: strings.NewReader("12345678"), Size: 8})
	if err != nil {
		t.Fatal(err)
	}
	newObject, err := service.Put(ctx, PutRequest{Key: "same.bin", Body: strings.NewReader("abcdefghi"), Size: 9})
	if err != nil {
		t.Fatal(err)
	}
	if oldObject.BucketID == newObject.BucketID {
		t.Fatalf("overwrite did not switch buckets: old=%s new=%s", oldObject.BucketID, newObject.BucketID)
	}
	cleanups, err := store.ListPhysicalCleanups(ctx, 10)
	if err != nil || len(cleanups) != 1 {
		t.Fatalf("cleanups = %#v, error = %v", cleanups, err)
	}
	oldBucket, _ := store.GetBucket(ctx, oldObject.BucketID)
	if oldBucket.StorageBytes != 8 {
		t.Fatalf("old bucket usage changed before cleanup: %d", oldBucket.StorageBytes)
	}
	physical := oldBucket.Name + "/same.bin"
	if _, err := store.db.ExecContext(ctx, `UPDATE r2_physical_cleanups SET expected_etag = '' WHERE id = ?`, cleanups[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ProcessPhysicalCleanups(ctx, 10); err == nil {
		t.Fatal("missing cleanup ETag should prevent cleanup")
	}
	if _, ok := backend.objects[physical]; !ok {
		t.Fatal("object with an unguarded cleanup was deleted")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE r2_physical_cleanups SET expected_etag = ? WHERE id = ?`, oldObject.ETag, cleanups[0].ID); err != nil {
		t.Fatal(err)
	}
	backend.etags[physical] = "newer-version"
	if _, err := service.ProcessPhysicalCleanups(ctx, 10); err == nil {
		t.Fatal("ETag mismatch should prevent cleanup")
	}
	if _, ok := backend.objects[physical]; !ok {
		t.Fatal("ETag-mismatched object was deleted")
	}
	backend.etags[physical] = oldObject.ETag
	completed, err := service.ProcessPhysicalCleanups(ctx, 10)
	if err != nil || completed != 1 {
		t.Fatalf("completed = %d, error = %v", completed, err)
	}
	oldBucket, _ = store.GetBucket(ctx, oldObject.BucketID)
	if oldBucket.StorageBytes != 0 {
		t.Fatalf("old bucket usage after cleanup = %d", oldBucket.StorageBytes)
	}
}

func TestPhysicalCleanupRetriesFairlyAndContinuesBatch(t *testing.T) {
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 200, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "primary")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishBucketScan(ctx, bucket.ID, 8, false); err != nil {
		t.Fatal(err)
	}
	backend := &memoryBackend{
		objects: map[string][]byte{
			"cleanup/stuck.bin": []byte("old1"),
			"cleanup/valid.bin": []byte("old2"),
		},
		etags: map[string]string{
			"cleanup/stuck.bin": "stuck-etag",
			"cleanup/valid.bin": "valid-etag",
		},
	}
	service := Service{Index: store, Accounts: accountStore, Backend: backend, TempDir: t.TempDir()}
	for _, statement := range []struct {
		id, key, etag string
		created       int64
	}{
		{id: "stuck", key: "stuck.bin", etag: "", created: 1},
		{id: "valid", key: "valid.bin", etag: "valid-etag", created: 2},
	} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO r2_physical_cleanups(
			id, object_key, physical_bucket_id, physical_key, expected_etag, size, status, error, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, 4, 'pending', '', ?, ?)`, statement.id, statement.key, bucket.ID,
			statement.key, statement.etag, statement.created, statement.created); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RetryPhysicalCleanup(ctx, "stuck", errors.New("retry later")); err != nil {
		t.Fatal(err)
	}
	queued, err := store.ListPhysicalCleanups(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 2 || queued[0].ID != "valid" || queued[1].ID != "stuck" {
		t.Fatalf("cleanup retry order = %#v", queued)
	}

	completed, err := service.ProcessPhysicalCleanups(ctx, 10)
	if err == nil || completed != 1 {
		t.Fatalf("completed = %d, error = %v", completed, err)
	}
	if _, ok := backend.objects["cleanup/valid.bin"]; ok {
		t.Fatal("valid cleanup object remains")
	}
	if _, ok := backend.objects["cleanup/stuck.bin"]; !ok {
		t.Fatal("unfenced cleanup object was deleted")
	}
	queued, err = store.ListPhysicalCleanups(ctx, 10)
	if err != nil || len(queued) != 1 || queued[0].ID != "stuck" {
		t.Fatalf("remaining cleanups = %#v, error = %v", queued, err)
	}
	bucket, err = store.GetBucket(ctx, bucket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bucket.StorageBytes != 4 {
		t.Fatalf("storage after partial cleanup = %d", bucket.StorageBytes)
	}
}
