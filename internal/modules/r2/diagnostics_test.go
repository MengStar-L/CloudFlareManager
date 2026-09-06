package r2

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type unavailableAccessBackend struct {
	*memoryBackend
	err       error
	failedKey string
}

func (b *unavailableAccessBackend) ListRemote(context.Context, Target, string, string, int32) (RemoteObjectList, error) {
	return RemoteObjectList{}, b.err
}

func (b *unavailableAccessBackend) Head(ctx context.Context, target Target, key string) (RemoteObject, error) {
	if b.failedKey == key {
		return RemoteObject{}, b.err
	}
	return b.memoryBackend.Head(ctx, target, key)
}

func TestFailedScanBlocksPlacementUntilSuccessfulScan(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	buckets, err := service.Index.ListBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bucket := buckets[0]
	service.Backend = &unavailableAccessBackend{memoryBackend: backend, err: classifyAWSMutationError(testRemoteError{code: "SignatureDoesNotMatch", status: 403})}
	if _, err := service.AdoptBucket(ctx, bucket.ID); !errors.Is(err, ErrR2Authentication) {
		t.Fatalf("scan error = %v", err)
	}
	if err := service.Index.ApplyAccountCapacity(ctx, bucket.AccountID, map[string]int64{bucket.ID: 0}, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	current, _ := service.Index.GetBucket(ctx, bucket.ID)
	if current.HealthStatus != "error" {
		t.Fatalf("capacity sync cleared failed scan: %#v", current)
	}
	if _, err := service.Put(ctx, PutRequest{Key: "test.txt", Body: strings.NewReader("test"), Size: 4}); !errors.Is(err, ErrBucketUnavailable) {
		t.Fatalf("failed bucket accepted write: %v", err)
	}
	account, err := service.Accounts.Get(ctx, bucket.AccountID, true)
	if err != nil {
		t.Fatal(err)
	}
	checks := service.DetectAccountAccess(ctx, account)
	if len(checks) != 1 || checks[0].Available || !strings.Contains(checks[0].Detail, "SignatureDoesNotMatch") {
		t.Fatalf("S3 capability = %#v", checks)
	}
	service.Backend = backend
	if _, err := service.ScanOrphans(ctx, bucket.ID); err != nil {
		t.Fatal(err)
	}
	if checks := service.DetectAccountAccess(ctx, account); len(checks) != 1 || !checks[0].Available {
		t.Fatalf("repaired S3 capability = %#v", checks)
	}
	if _, err := service.Put(ctx, PutRequest{Key: "test.txt", Body: strings.NewReader("test"), Size: 4}); err != nil {
		t.Fatalf("repaired bucket rejected write: %v", err)
	}
}

func TestRecoveryFailureDoesNotStarveOtherWrites(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	for _, key := range []string{"first-failing", "second-recoverable"} {
		intent, err := service.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: key, Size: 0}})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Index.HoldWriteForRecovery(ctx, intent.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	service.Backend = &unavailableAccessBackend{memoryBackend: backend, failedKey: "first-failing", err: classifyAWSMutationError(testRemoteError{code: "AccessDenied", status: 403})}
	err := service.RecoverInterruptedBeforeServing(ctx)
	var pending *RecoveryPendingError
	if !errors.As(err, &pending) || len(pending.Failures) != 1 {
		t.Fatalf("recovery result = %v", err)
	}
	intents, err := service.Index.ListWriteIntents(ctx, 10)
	if err != nil || len(intents) != 1 || intents[0].Key != "first-failing" || intents[0].LastError == "" {
		t.Fatalf("remaining recovery = %#v, %v", intents, err)
	}
}
