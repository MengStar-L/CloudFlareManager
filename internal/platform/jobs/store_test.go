package jobs

import (
	"context"
	"path/filepath"
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
	if err := store.Fail(ctx, job.ID, "temporary failure", time.Now().Add(-time.Second)); err != nil {
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
	if got.Status != StatusSucceeded || got.Progress != 1 {
		t.Fatalf("completed job = %#v", got)
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
