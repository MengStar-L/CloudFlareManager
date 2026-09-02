package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
)

func TestRunnerPermanentlyFailsClassifiedError(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	job, err := store.Enqueue(context.Background(), "permanent", nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(store)
	runner.Register("permanent", func(context.Context, Job) error {
		return NewFailure("permission_denied", errors.New("permission denied"), true)
	})

	worked, err := runner.runOne(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked {
		t.Fatal("runner did not claim the job")
	}
	failed, err := store.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != StatusFailed || failed.Attempts != 1 || failed.ErrorCode != "permission_denied" || failed.LeaseUntil != nil {
		t.Fatalf("failed job = %#v", failed)
	}
}

func TestRunnerRetriesClassifiedTransientError(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	job, err := store.Enqueue(context.Background(), "transient", nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(store)
	runner.RetryPolicy.BaseDelay = time.Minute
	runner.Register("transient", func(context.Context, Job) error {
		return NewFailure("rate_limited", errors.New("rate limited"), false)
	})

	worked, err := runner.runOne(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked {
		t.Fatal("runner did not claim the job")
	}
	pending, err := store.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != StatusPending || pending.Attempts != 1 || pending.ErrorCode != "rate_limited" || pending.LeaseUntil == nil {
		t.Fatalf("pending job = %#v", pending)
	}
	if time.Until(*pending.LeaseUntil) < 50*time.Second {
		t.Fatalf("retry timestamp = %v", pending.LeaseUntil)
	}
}
