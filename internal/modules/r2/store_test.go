package r2

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

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
