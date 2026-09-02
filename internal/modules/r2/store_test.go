package r2

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestCreateBucketBlockedByActiveRemoteDeletionJob(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "delete-race")
	jobStore := jobs.NewStore(store.db)
	resourceKey := account.ID + "/default/gamesync"
	job, created, err := jobStore.EnqueueUnique(ctx, BucketDeletionJobType, resourceKey, "", map[string]string{"bucket_name": "gamesync"}, 4)
	if err != nil || !created {
		t.Fatalf("enqueue remote deletion job: created=%v, err=%v", created, err)
	}

	if _, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "gamesync"}); !errors.Is(err, ErrBucketDeleting) {
		t.Fatalf("create bucket during remote deletion error = %v", err)
	}
	if _, err := store.GetBucketByAccountAndName(ctx, account.ID, "gamesync"); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("bucket was registered despite active deletion job: %v", err)
	}

	claimed, err := jobStore.Claim(ctx, 0)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claim deletion job: %#v, %v", claimed, err)
	}
	if err := jobStore.FailPermanent(ctx, job.ID, "test_complete", "terminal"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "gamesync"}); err != nil {
		t.Fatalf("create bucket after deletion job became terminal: %v", err)
	}
}

func TestStoreObjectStateLifecycle(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{3}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
		R2AccessKeyID: "access", R2SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, Limits{StorageBytes: 1000, ClassA: 100, ClassB: 100})
	bucket, err := store.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "physical"})
	if err != nil {
		t.Fatal(err)
	}

	object, err := store.ReservePut(context.Background(), ObjectInput{Key: "docs/readme.txt", Size: 12, ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if object.State != StatePending || object.BucketID != bucket.ID {
		t.Fatalf("reserved object = %#v", object)
	}
	if err := store.CommitPut(context.Background(), object.ObjectID, "etag", 12); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetObject(context.Background(), object.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCommitted || got.ETag != "etag" {
		t.Fatalf("committed object = %#v", got)
	}
	if err := store.BeginDelete(context.Background(), object.Key); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteDelete(context.Background(), object.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetObject(context.Background(), object.Key); err != ErrObjectNotFound {
		t.Fatalf("get deleted object error = %v", err)
	}
}

func TestStoreBackfillObjectETagIsCompareAndSet(t *testing.T) {
	t.Parallel()
	store, accountsStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountsStore, ctx, "primary")
	if _, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "physical"}); err != nil {
		t.Fatal(err)
	}
	object, err := store.ReservePut(ctx, ObjectInput{Key: "legacy.txt", Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitPut(ctx, object.ObjectID, "original-etag", 4); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE r2_objects SET etag = '' WHERE object_id = ?", object.ObjectID); err != nil {
		t.Fatal(err)
	}

	repaired, err := store.BackfillObjectETag(ctx, object.ObjectID, "first-etag")
	if err != nil {
		t.Fatal(err)
	}
	if repaired.ETag != "first-etag" {
		t.Fatalf("repaired object = %#v", repaired)
	}
	winner, err := store.BackfillObjectETag(ctx, object.ObjectID, "first-etag")
	if err != nil || winner.ETag != "first-etag" {
		t.Fatalf("idempotent repair = %#v, error = %v", winner, err)
	}
	if _, err := store.BackfillObjectETag(ctx, object.ObjectID, "second-etag"); !errors.Is(err, ErrConditionalRequestConflict) {
		t.Fatalf("conflicting repair error = %v", err)
	}
	if _, err := store.BackfillObjectETag(ctx, object.ObjectID, ""); err != ErrObjectETagUnavailable {
		t.Fatalf("empty ETag error = %v", err)
	}
	if _, err := store.BackfillObjectETag(ctx, "missing-object", "etag"); err != ErrObjectNotFound {
		t.Fatalf("missing object error = %v", err)
	}
}

func TestStoreBackfillObjectMetadataRejectsStaleSize(t *testing.T) {
	t.Parallel()
	store, accountsStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountsStore, ctx, "primary")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "physical"})
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.ReservePut(ctx, ObjectInput{Key: "legacy.txt", Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitPut(ctx, object.ObjectID, "original-etag", 4); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE r2_objects SET etag = '', size = 2 WHERE object_id = ?`, object.ObjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BackfillObjectMetadata(ctx, object.ObjectID, "", 4, "remote-etag", 8); !errors.Is(err, ErrConditionalRequestConflict) {
		t.Fatalf("BackfillObjectMetadata error = %v", err)
	}
	current, err := store.GetObjectByID(ctx, object.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Size != 2 || current.ETag != "" {
		t.Fatalf("stale repair changed object: %#v", current)
	}
	currentBucket, err := store.GetBucket(ctx, bucket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentBucket.StorageBytes != bucket.StorageBytes {
		t.Fatalf("stale repair changed storage: before=%d after=%d", bucket.StorageBytes, currentBucket.StorageBytes)
	}
}

func TestStoreListsCommittedObjectsByPrefix(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{4}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, Limits{StorageBytes: 1000, ClassA: 100, ClassB: 100})
	if _, err := store.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "physical"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"docs/a.txt", "docs/b.txt", "images/c.png"} {
		object, err := store.ReservePut(context.Background(), ObjectInput{Key: key, Size: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CommitPut(context.Background(), object.ObjectID, key, 1); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.ListObjects(context.Background(), ListOptions{Prefix: "docs/", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(items.Objects) != 2 || items.Objects[0].Key != "docs/a.txt" {
		t.Fatalf("objects = %#v", items.Objects)
	}
}

func TestStoreListBucketObjectStatsReflectsCommittedIndex(t *testing.T) {
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
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, Limits{StorageBytes: 1000, ClassA: 100, ClassB: 100})
	source, err := store.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "source"})
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.ReservePut(context.Background(), ObjectInput{Key: "first.bin", Size: 12})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitPut(context.Background(), first.ObjectID, "first-etag", 12); err != nil {
		t.Fatal(err)
	}
	second, err := store.ReservePut(context.Background(), ObjectInput{Key: "second.bin", Size: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitPut(context.Background(), second.ObjectID, "second-etag", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReservePut(context.Background(), ObjectInput{Key: "pending.bin", Size: 99}); err != nil {
		t.Fatal(err)
	}

	stats, err := store.ListBucketObjectStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := stats[source.ID]; got.StorageBytes != 17 || got.ObjectCount != 2 {
		t.Fatalf("source stats = %#v", got)
	}

	replacement, err := store.ReservePut(context.Background(), ObjectInput{Key: "first.bin", Size: 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitPut(context.Background(), replacement.ObjectID, "replacement-etag", 20); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginDelete(context.Background(), "second.bin"); err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MoveObjectMapping(context.Background(), replacement.ObjectID, target.ID, "moved-etag"); err != nil {
		t.Fatal(err)
	}

	stats, err = store.ListBucketObjectStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stats[source.ID]; ok {
		t.Fatalf("source should have no committed objects: %#v", stats[source.ID])
	}
	if got := stats[target.ID]; got.StorageBytes != 20 || got.ObjectCount != 1 {
		t.Fatalf("target stats = %#v", got)
	}
}
