package r2

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func newIntentTestStore(t *testing.T, limits Limits) (*Store, *accounts.Store, context.Context) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{17}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(db, limits), accounts.NewStore(db, secret.NewRepository(db, cipher)), context.Background()
}

func createIntentAccount(t *testing.T, store *accounts.Store, ctx context.Context, name string) accounts.Account {
	t.Helper()
	account, err := store.Create(ctx, accounts.CreateInput{
		Name: name, CloudflareAccountID: name + "-cloudflare", APIToken: "token",
		R2AccessKeyID: "access", R2SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func TestWriteIntentUsesConfiguredLimitWithoutHiddenDiscount(t *testing.T) {
	t.Parallel()
	store, accountsStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 100, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountsStore, ctx, "primary")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishBucketScan(ctx, bucket.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE r2_physical_buckets SET storage_bytes = 90 WHERE id = ?", bucket.ID); err != nil {
		t.Fatal(err)
	}
	intent, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "exact.bin", Size: 10}, ExpectedClassA: 1})
	if err != nil {
		t.Fatal(err)
	}
	if intent.BucketID != bucket.ID || intent.ReservedBytes != 10 {
		t.Fatalf("intent = %#v", intent)
	}
	if _, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "over.bin", Size: 1}, ExpectedClassA: 1}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over-limit error = %v", err)
	}
}

func TestWriteIntentRejectsBucketBeforeInitialUsageSync(t *testing.T) {
	t.Parallel()
	store, accountsStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 100, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountsStore, ctx, "primary")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "blocked.bin", Size: 1}}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("unsynchronized bucket error = %v", err)
	}
	if err := store.FinishBucketScan(ctx, bucket.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "ready.bin", Size: 1}}); err != nil {
		t.Fatalf("synchronized bucket should be eligible: %v", err)
	}
}

func TestWriteIntentFencesAndRestoresAccountR2Credentials(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 100, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "primary")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishBucketScan(ctx, bucket.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := accountStore.UpdateCredentials(ctx, account.ID, accounts.UpdateCredentialsInput{ClearR2Credentials: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginWrite(ctx, BeginWriteInput{
		ObjectInput: ObjectInput{Key: "blocked.bin", Size: 1}, TargetBucketID: bucket.ID,
	}); !errors.Is(err, ErrR2CredentialsRequired) {
		t.Fatalf("write with removed R2 credentials error = %v, want ErrR2CredentialsRequired", err)
	}
	if _, err := store.BeginWrite(ctx, BeginWriteInput{
		ObjectInput: ObjectInput{Key: "auto-blocked.bin", Size: 1},
	}); !errors.Is(err, ErrR2CredentialsRequired) {
		t.Fatalf("automatic placement with removed R2 credentials error = %v, want ErrR2CredentialsRequired", err)
	}
	accessKey, secretKey := "replacement-access", "replacement-secret"
	if _, err := accountStore.UpdateCredentials(ctx, account.ID, accounts.UpdateCredentialsInput{
		R2AccessKeyID: &accessKey, R2SecretAccessKey: &secretKey,
	}); err != nil {
		t.Fatal(err)
	}
	intent, err := store.BeginWrite(ctx, BeginWriteInput{
		ObjectInput: ObjectInput{Key: "restored.bin", Size: 1}, TargetBucketID: bucket.ID,
	})
	if err != nil {
		t.Fatalf("write after restoring R2 credentials: %v", err)
	}
	if intent.BucketID != bucket.ID {
		t.Fatalf("restored write target = %q, want %q", intent.BucketID, bucket.ID)
	}
}

func TestConcurrentWriteReservationsDoNotOversell(t *testing.T) {
	t.Parallel()
	store, accountsStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 100, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountsStore, ctx, "primary")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishBucketScan(ctx, bucket.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var group sync.WaitGroup
	for _, key := range []string{"one.bin", "two.bin"} {
		key := key
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: key, Size: 60}})
			errorsCh <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsCh)
	var succeeded, rejected int
	for err := range errorsCh {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrQuotaExceeded) {
			rejected++
		} else {
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("succeeded=%d rejected=%d", succeeded, rejected)
	}
	updated, err := store.GetBucket(ctx, bucket.ID)
	if err != nil || updated.ReservedBytes != 60 {
		t.Fatalf("bucket = %#v, error = %v", updated, err)
	}
}

func TestWriteIntentSwitchesAccountsWhenAggregateCapacityIsExhausted(t *testing.T) {
	t.Parallel()
	store, accountsStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 100, ClassA: 100, ClassB: 100})
	firstAccount := createIntentAccount(t, accountsStore, ctx, "first")
	secondAccount := createIntentAccount(t, accountsStore, ctx, "second")
	first, _ := store.CreateBucket(ctx, CreateBucketInput{AccountID: firstAccount.ID, Name: "first"})
	second, _ := store.CreateBucket(ctx, CreateBucketInput{AccountID: secondAccount.ID, Name: "second"})
	_ = store.FinishBucketScan(ctx, first.ID, 0, false)
	_ = store.FinishBucketScan(ctx, second.ID, 0, false)
	if _, err := store.db.ExecContext(ctx, "UPDATE r2_physical_buckets SET storage_bytes = 20 WHERE id = ?", first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUnmanagedStorage(ctx, firstAccount.ID, 75, timeNowForTest()); err != nil {
		t.Fatal(err)
	}
	intent, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "switch.bin", Size: 10}, ExpectedClassA: 1})
	if err != nil {
		t.Fatal(err)
	}
	if intent.BucketID != second.ID {
		t.Fatalf("selected bucket = %s, want %s", intent.BucketID, second.ID)
	}
}

func TestWriteIntentFallsBackWhenRuleTargetBucketIsFull(t *testing.T) {
	t.Parallel()
	store, accountsStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 200, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountsStore, ctx, "primary")
	full, _ := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "full"})
	available, _ := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "available"})
	_ = store.FinishBucketScan(ctx, full.ID, 95, false)
	_ = store.FinishBucketScan(ctx, available.ID, 0, false)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO r2_placement_rules(
		id, priority, prefix, extension, content_type, min_size, max_size, target_bucket_id, enabled, created_at, updated_at)
		VALUES('prefer-full', 1, 'rules/', '', '', 0, 0, ?, 1, 1, 1)`, full.ID); err != nil {
		t.Fatal(err)
	}
	intent, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "rules/file.bin", Size: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if intent.BucketID != available.ID {
		t.Fatalf("selected bucket = %s, want fallback %s", intent.BucketID, available.ID)
	}
}

func TestWriteIntentPreservesCommittedObjectUntilCommit(t *testing.T) {
	t.Parallel()
	store, accountsStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 1000, AccountStorageBytes: 1000, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountsStore, ctx, "primary")
	bucket, _ := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "bucket"})
	_ = store.FinishBucketScan(ctx, bucket.ID, 0, false)
	legacy, err := store.ReservePut(ctx, ObjectInput{Key: "file.txt", Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitPut(ctx, legacy.ObjectID, "old-etag", 3); err != nil {
		t.Fatal(err)
	}
	intent, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "file.txt", Size: 4}, ExpectedClassA: 1})
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.GetObject(ctx, "file.txt")
	if err != nil || current.ObjectID != legacy.ObjectID {
		t.Fatalf("current object = %#v, error = %v", current, err)
	}
	if _, err := store.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: "file.txt", Size: 4}}); !errors.Is(err, ErrWriteInProgress) {
		t.Fatalf("concurrent error = %v", err)
	}
	if err := store.AbortWrite(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	current, err = store.GetObject(ctx, "file.txt")
	if err != nil || current.ETag != "old-etag" {
		t.Fatalf("object after abort = %#v, error = %v", current, err)
	}
}

func timeNowForTest() time.Time { return time.Unix(1_700_000_000, 0) }
