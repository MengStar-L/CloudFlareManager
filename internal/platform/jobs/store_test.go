package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
)

func TestStoreJobLifecycle(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	ctx := context.Background()

	job, err := store.Enqueue(ctx, "account.capabilities.detect", map[string]string{"account_id": "a1"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != job.ID || claimed.Status != StatusRunning || claimed.Attempts != 1 {
		t.Fatalf("claimed job = %#v", claimed)
	}
	if err := store.SetProgress(ctx, job.ID, .5); err != nil {
		t.Fatal(err)
	}
	if err := store.Fail(ctx, job.ID, "rate_limited", "temporary failure", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.Attempts != 2 {
		t.Fatalf("reclaimed job = %#v", claimed)
	}
	if err := store.Complete(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded || got.Progress != 1 || got.Error != "" || got.ErrorCode != "" {
		t.Fatalf("completed job = %#v", got)
	}
}

func TestEnqueueForAccountRequiresExistingAccount(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	ctx := context.Background()

	if _, err := store.EnqueueForAccount(ctx, "missing", "account.capabilities.detect", nil, 3); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing account enqueue error = %v, want ErrAccountNotFound", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO encrypted_secrets(id, scope, kind, ciphertext, created_at, updated_at)
		VALUES('secret', 'account:account', 'cloudflare_api_token', X'00', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO accounts(id, name, cloudflare_account_id, api_token_secret_id, created_at, updated_at)
		VALUES('account', 'account', 'cloudflare', 'secret', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	job, err := store.EnqueueForAccount(ctx, "account", "account.capabilities.detect", map[string]string{"account_id": "account"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if job.Type != "account.capabilities.detect" {
		t.Fatalf("job = %#v", job)
	}
	items, err := store.List(ctx, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != job.ID {
		t.Fatalf("jobs = %#v, want only %q", items, job.ID)
	}
}

func TestEnqueueUniqueDeduplicatesActiveResource(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	ctx := context.Background()

	type result struct {
		job     Job
		created bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			job, created, err := store.EnqueueUnique(ctx, "r2.bucket.delete-remote", "account/default/bucket", "", map[string]string{"bucket": "bucket"}, 4)
			results <- result{job: job, created: created, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	var jobID string
	createdCount := 0
	for got := range results {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if jobID == "" {
			jobID = got.job.ID
		} else if got.job.ID != jobID {
			t.Fatalf("deduplicated job IDs differ: %q and %q", jobID, got.job.ID)
		}
		if got.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count = %d, want 1", createdCount)
	}
	jobs, err := store.List(ctx, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ResourceKey != "account/default/bucket" {
		t.Fatalf("jobs = %#v", jobs)
	}
}

func TestEnqueueUniqueAllowsResourceAfterTerminalJob(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	ctx := context.Background()

	first, created, err := store.EnqueueUnique(ctx, "r2.bucket.delete-remote", "account/default/bucket", "", nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first resource job was not created")
	}
	if _, err := store.Claim(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, first.ID); err != nil {
		t.Fatal(err)
	}

	second, created, err := store.EnqueueUnique(ctx, "r2.bucket.delete-remote", "account/default/bucket", first.ID, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !created || second.ID == first.ID {
		t.Fatalf("second job = %#v, created=%v", second, created)
	}
	if second.ParentJobID != first.ID {
		t.Fatalf("parent job ID = %q, want %q", second.ParentJobID, first.ID)
	}
}

func TestListFilteredAppliesFiltersBeforeLimit(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	ctx := context.Background()

	wanted, created, err := store.EnqueueUnique(ctx, "r2.bucket.delete-remote", "account/default/gamesync", "", nil, 4)
	if err != nil || !created {
		t.Fatalf("enqueue wanted job: created=%v err=%v", created, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET created_at = 1 WHERE id = ?`, wanted.ID); err != nil {
		t.Fatal(err)
	}
	for range 501 {
		if _, err := store.Enqueue(ctx, "unrelated", nil, 1); err != nil {
			t.Fatal(err)
		}
	}

	items, err := store.ListFiltered(ctx, 1, StatusPending, "r2.bucket.delete-remote", "account/default/")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != wanted.ID {
		t.Fatalf("filtered jobs = %#v, want %q", items, wanted.ID)
	}
}

func TestFailPermanentPersistsCodeWithoutRetry(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	ctx := context.Background()
	job, err := store.Enqueue(ctx, "test", nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.FailPermanent(ctx, job.ID, "permission_denied", "permission denied"); err != nil {
		t.Fatal(err)
	}
	failed, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != StatusFailed || failed.ErrorCode != "permission_denied" || failed.Error != "permission denied" || failed.LeaseUntil != nil {
		t.Fatalf("failed job = %#v", failed)
	}
	claimed, err := store.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed != nil {
		t.Fatalf("permanently failed job was reclaimed: %#v", claimed)
	}
}

func TestClaimRecoversExpiredLease(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	ctx := context.Background()
	job, err := store.Enqueue(ctx, "test", nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(ctx, -time.Second); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != job.ID || claimed.Attempts != 2 {
		t.Fatalf("expired lease was not recovered: %#v", claimed)
	}
}

func TestClaimRecoversExpiredLeaseAtMaximumAttempts(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	ctx := context.Background()
	job, err := store.Enqueue(ctx, "r2.bucket.delete-remote", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Claim(ctx, -time.Second)
	if err != nil || first == nil || first.Attempts != first.MaxAttempts {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	recovered, err := store.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || recovered.ID != job.ID || recovered.Attempts != recovered.MaxAttempts {
		t.Fatalf("final-attempt lease was not recovered: %#v", recovered)
	}
}

func TestRunningJobLeaseCanBeRenewed(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	job, err := store.Enqueue(context.Background(), "test", nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := store.RenewLease(context.Background(), job.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	renewed, err := store.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.LeaseUntil == nil || time.Until(*renewed.LeaseUntil) < 59*time.Minute {
		t.Fatalf("renewed lease = %#v", renewed.LeaseUntil)
	}
}
