package r2

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
)

type fakeUsageProvider struct {
	usage map[string]map[string]accounts.BucketUsage
}

func (p fakeUsageProvider) R2BucketUsage(_ context.Context, accountID, _ string) (map[string]accounts.BucketUsage, error) {
	return p.usage[accountID], nil
}

func TestSyncAccountCapacityIncludesManagedAndUnmanagedBuckets(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 200, ClassA: 100, ClassB: 100})
	account := createIntentAccount(t, accountStore, ctx, "primary")
	bucket, err := store.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "managed"})
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Index: store, Accounts: accountStore, Usage: fakeUsageProvider{usage: map[string]map[string]accounts.BucketUsage{
		account.CloudflareAccountID: {
			"managed":   {PayloadBytes: 10, MetadataBytes: 1},
			"unmanaged": {PayloadBytes: 5, MetadataBytes: 2},
		},
	}}}
	if err := service.SyncAccountCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	updated, err := store.GetBucket(ctx, bucket.ID)
	if err != nil || updated.StorageBytes != 11 || updated.UsageCheckedAt.IsZero() {
		t.Fatalf("bucket = %#v, error = %v", updated, err)
	}
	usage, err := store.GetAccountUsage(ctx, account.ID)
	if err != nil || usage.ManagedBytes != 11 || usage.UnmanagedBytes != 7 {
		t.Fatalf("usage = %#v, error = %v", usage, err)
	}
}

func TestAccountOperationLimitsAndUTCMonth(t *testing.T) {
	t.Parallel()
	store, accountStore, ctx := newIntentTestStore(t, Limits{StorageBytes: 100, AccountStorageBytes: 100, ClassA: 1, ClassB: 1})
	account := createIntentAccount(t, accountStore, ctx, "primary")
	if err := store.ConsumeOperation(ctx, account.ID, OperationClassA); err != nil {
		t.Fatal(err)
	}
	if err := store.ConsumeOperation(ctx, account.ID, OperationClassA); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("second Class A operation error = %v", err)
	}
	beijing := time.FixedZone("UTC+8", 8*60*60)
	if got := usageMonth(time.Date(2026, 8, 1, 0, 30, 0, 0, beijing)); got != "2026-07" {
		t.Fatalf("UTC usage month = %q", got)
	}
}
