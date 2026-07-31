package r2

import (
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
