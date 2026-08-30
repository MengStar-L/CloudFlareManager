package webdavprotocol

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	platformdb "github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
)

func TestLockStoreTokenCoversDepthAndPathBoundaries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestLockStore(t)
	depthZero := createTestLock(t, store, "mount/docs/", "0")
	depthInfinity := createTestLock(t, store, "mount/images/", "infinity")

	assertTokenCovers(t, store, depthZero.Token, "mount/docs/", true)
	assertTokenCovers(t, store, depthZero.Token, "mount/docs/readme.txt", false)
	assertTokenCovers(t, store, depthInfinity.Token, "mount/images/photo.jpg", true)
	assertTokenCovers(t, store, depthInfinity.Token, "mount/images-old/photo.jpg", false)

	covered, err := store.TokenCovers(ctx, "opaquelocktoken:missing", "mount/docs/")
	if covered || !errors.Is(err, ErrLockNotFound) {
		t.Fatalf("TokenCovers missing token = %v, %v; want false, ErrLockNotFound", covered, err)
	}
}

func TestLockStoreCheckPathsRequiresEveryCoveringToken(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestLockStore(t)
	fileLock := createTestLock(t, store, "mount/file.txt", "0")
	treeLock := createTestLock(t, store, "mount/tree/", "infinity")
	paths := []string{"mount/file.txt", "mount/tree/child.txt"}

	if err := store.CheckPaths(ctx, paths, []string{fileLock.Token}); !errors.Is(err, ErrLocked) {
		t.Fatalf("CheckPaths with one token = %v, want ErrLocked", err)
	}
	if err := store.CheckPaths(ctx, paths, []string{fileLock.Token, treeLock.Token}); err != nil {
		t.Fatalf("CheckPaths with all tokens: %v", err)
	}
	if err := store.CheckPaths(ctx, paths, []string{"opaquelocktoken:unrelated", treeLock.Token, fileLock.Token}); err != nil {
		t.Fatalf("CheckPaths with all tokens plus unrelated token: %v", err)
	}

	if err := store.Check(ctx, "mount/file.txt", fileLock.Token); err != nil {
		t.Fatalf("legacy Check with matching token: %v", err)
	}
	if err := store.Check(ctx, "mount/file.txt", ""); !errors.Is(err, ErrLocked) {
		t.Fatalf("legacy Check without token = %v, want ErrLocked", err)
	}
}

func TestLockStorePurgesExpiredLocks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db := newTestLockStore(t)
	now := time.Now().Unix()
	insertTestLock(t, db, "opaquelocktoken:expired-check", "mount/expired-check", "0", now-1)
	insertTestLock(t, db, "opaquelocktoken:expired-scope", "mount/expired-scope", "infinity", now-1)
	insertTestLock(t, db, "opaquelocktoken:active", "mount/active", "0", now+60)

	if err := store.CheckPaths(ctx, []string{"mount/expired-check"}, nil); err != nil {
		t.Fatalf("CheckPaths on expired lock: %v", err)
	}
	assertLockRowCount(t, db, "opaquelocktoken:expired-check", 0)

	covered, err := store.TokenCovers(ctx, "opaquelocktoken:expired-scope", "mount/expired-scope/child")
	if covered || !errors.Is(err, ErrLockNotFound) {
		t.Fatalf("TokenCovers expired token = %v, %v; want false, ErrLockNotFound", covered, err)
	}
	assertLockRowCount(t, db, "opaquelocktoken:expired-scope", 0)
	assertLockRowCount(t, db, "opaquelocktoken:active", 1)
}

func TestLockStoreCreateRejectsOverlappingExclusiveLocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		existingKey   string
		existingDepth string
		newKey        string
		newDepth      string
		wantLocked    bool
	}{
		{name: "same resource", existingKey: "mount/docs/", existingDepth: "0", newKey: "mount/docs/", newDepth: "0", wantLocked: true},
		{name: "infinity ancestor then child", existingKey: "mount/docs/", existingDepth: "infinity", newKey: "mount/docs/readme.txt", newDepth: "0", wantLocked: true},
		{name: "child then infinity ancestor", existingKey: "mount/docs/readme.txt", existingDepth: "0", newKey: "mount/docs/", newDepth: "infinity", wantLocked: true},
		{name: "depth zero parent and child", existingKey: "mount/docs/", existingDepth: "0", newKey: "mount/docs/readme.txt", newDepth: "0"},
		{name: "segment prefix sibling", existingKey: "mount/docs", existingDepth: "infinity", newKey: "mount/docs-old/readme.txt", newDepth: "0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, _ := newTestLockStore(t)
			createTestLock(t, store, tt.existingKey, tt.existingDepth)
			_, err := store.Create(context.Background(), tt.newKey, "owner", tt.newDepth, time.Minute)
			if tt.wantLocked && !errors.Is(err, ErrLocked) {
				t.Fatalf("Create(%q, %q) = %v, want ErrLocked", tt.newKey, tt.newDepth, err)
			}
			if !tt.wantLocked && err != nil {
				t.Fatalf("Create(%q, %q): %v", tt.newKey, tt.newDepth, err)
			}
		})
	}
}

func TestLockStoreConcurrentCreateAllowsOneExclusiveLock(t *testing.T) {
	t.Parallel()

	store, _ := newTestLockStore(t)
	const contenders = 64
	start := make(chan struct{})
	results := make(chan error, contenders)
	var workers sync.WaitGroup
	workers.Add(contenders)
	for range contenders {
		go func() {
			defer workers.Done()
			<-start
			_, err := store.Create(context.Background(), "mount/concurrent.txt", "owner", "0", time.Minute)
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	succeeded := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrLocked):
		default:
			t.Fatalf("concurrent Create returned unexpected error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent Create successes = %d, want 1", succeeded)
	}
}

func TestLockMutationGuardsSerializeOnlyOverlappingPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _ := newTestLockStore(t)
	guard, err := store.GuardPaths(ctx, []string{"mount/tree/file.txt"}, []string{"mount/tree/file.txt"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(guard.Release)

	unrelated, err := store.GuardPaths(ctx, []string{"other/file.txt"}, []string{"other/file.txt"}, nil, nil)
	if err != nil {
		t.Fatalf("unrelated guard: %v", err)
	}
	unrelated.Release()

	createResult := make(chan error, 1)
	go func() {
		_, createErr := store.Create(ctx, "mount/tree/file.txt", "owner", "0", time.Minute)
		createResult <- createErr
	}()
	assertNoLockResult(t, createResult, "overlapping LOCK")
	guard.Release()
	if err := waitLockResult(t, createResult, "overlapping LOCK after release"); err != nil {
		t.Fatal(err)
	}
}

func TestDescendantDepthZeroLockWaitsForActiveAncestorMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _ := newTestLockStore(t)
	guard, err := store.GuardPaths(ctx, []string{"mount/tree/"}, []string{"mount/tree/"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(guard.Release)

	result := make(chan error, 1)
	go func() {
		_, createErr := store.Create(ctx, "mount/tree/new.txt", "owner", "0", time.Minute)
		result <- createErr
	}()
	assertNoLockResult(t, result, "descendant depth-zero LOCK")
	guard.Release()
	if err := waitLockResult(t, result, "descendant depth-zero LOCK after release"); err != nil {
		t.Fatal(err)
	}
}

func TestLockMutationGuardsSerializeOverlappingMutationsAndReleaseOnFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _ := newTestLockStore(t)
	guard, err := store.GuardPaths(ctx, []string{"mount/tree/"}, []string{"mount/tree/"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	mutationResult := make(chan error, 1)
	go func() {
		nested, nestedErr := store.GuardPaths(ctx, []string{"mount/tree/child.txt"}, []string{"mount/tree/child.txt"}, nil, nil)
		if nested != nil {
			nested.Release()
		}
		mutationResult <- nestedErr
	}()
	assertNoLockResult(t, mutationResult, "overlapping mutation")
	guard.Release()
	if err := waitLockResult(t, mutationResult, "overlapping mutation after release"); err != nil {
		t.Fatal(err)
	}

	lock := createTestLock(t, store, "mount/locked.txt", "0")
	failed, err := store.GuardPaths(ctx, []string{lock.Key}, []string{lock.Key}, nil, nil)
	if failed != nil || !errors.Is(err, ErrLocked) {
		t.Fatalf("failed guard = %#v, %v", failed, err)
	}
	if err := store.Delete(ctx, lock.Token); err != nil {
		t.Fatalf("failed guard leaked mutex or mutation registration: %v", err)
	}
}

func TestLockCreationWaitHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	store, _ := newTestLockStore(t)
	guard, err := store.GuardPaths(context.Background(), []string{"mount/cancel.txt"}, []string{"mount/cancel.txt"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := store.Create(ctx, "mount/cancel.txt", "owner", "0", time.Minute); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Create cancellation error = %v", err)
	}
}

func TestLockStoreDeletePathsRemovesRootsAndDescendantsOnly(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestLockStore(t)
	removed := []Lock{
		createTestLock(t, store, "mount/docs", "0"),
		createTestLock(t, store, "mount/docs/child.txt", "0"),
		createTestLock(t, store, "mount/docs/nested/item.txt", "0"),
		createTestLock(t, store, "mount/archive/", "0"),
		createTestLock(t, store, "mount/archive/item.txt", "0"),
	}
	preserved := []Lock{
		createTestLock(t, store, "mount/doc", "0"),
		createTestLock(t, store, "mount/docs-old/item.txt", "0"),
		createTestLock(t, store, "mount/other/item.txt", "0"),
	}

	if err := store.DeletePaths(ctx, []string{"mount/docs", "mount/archive/"}); err != nil {
		t.Fatalf("DeletePaths: %v", err)
	}
	for _, lock := range removed {
		if _, err := store.Get(ctx, lock.Token); !errors.Is(err, ErrLockNotFound) {
			t.Errorf("Get(%q) after DeletePaths = %v, want ErrLockNotFound", lock.Key, err)
		}
	}
	for _, lock := range preserved {
		if _, err := store.Get(ctx, lock.Token); err != nil {
			t.Errorf("Get(%q) after DeletePaths: %v", lock.Key, err)
		}
	}
}

func TestLockStoreRelevantLockRootsReturnsOnlyAffectedAncestors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := newTestLockStore(t)
	parent := createTestLock(t, store, "mount/source/", "0")
	deleted := createTestLock(t, store, "mount/source/a.txt", "0")
	createTestLock(t, store, "mount/source/b.txt", "0")
	createTestLock(t, store, "mount/source-old/a.txt", "0")
	createTestLock(t, store, "other/source/a.txt", "0")

	roots, err := store.RelevantLockRoots(ctx, []string{"mount/source/a.txt"})
	if err != nil {
		t.Fatalf("RelevantLockRoots: %v", err)
	}
	got := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		got[root] = struct{}{}
	}
	for _, want := range []string{parent.Key, deleted.Key} {
		if _, ok := got[want]; !ok {
			t.Errorf("RelevantLockRoots missing %q: %v", want, roots)
		}
	}
	if len(got) != 2 {
		t.Fatalf("RelevantLockRoots = %v, want only affected root and ancestor", roots)
	}
}

func newTestLockStore(t *testing.T) (*LockStore, *sql.DB) {
	t.Helper()

	db, err := platformdb.Open(filepath.Join(t.TempDir(), "locks.db"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewLockStore(db), db
}

func createTestLock(t *testing.T, store *LockStore, key, depth string) Lock {
	t.Helper()

	lock, err := store.Create(context.Background(), key, "owner", depth, time.Minute)
	if err != nil {
		t.Fatalf("Create(%q, %q): %v", key, depth, err)
	}
	return lock
}

func insertTestLock(t *testing.T, db *sql.DB, token, key, depth string, expiresAt int64) {
	t.Helper()

	if _, err := db.Exec(`INSERT INTO webdav_locks(token, object_key, owner, depth, expires_at, created_at)
		VALUES(?, ?, 'owner', ?, ?, ?)`, token, key, depth, expiresAt, time.Now().Unix()); err != nil {
		t.Fatalf("insert test lock: %v", err)
	}
}

func assertTokenCovers(t *testing.T, store *LockStore, token, key string, want bool) {
	t.Helper()

	covered, err := store.TokenCovers(context.Background(), token, key)
	if err != nil {
		t.Fatalf("TokenCovers(%q, %q): %v", token, key, err)
	}
	if covered != want {
		t.Fatalf("TokenCovers(%q, %q) = %v, want %v", token, key, covered, want)
	}
}

func assertLockRowCount(t *testing.T, db *sql.DB, token string, want int) {
	t.Helper()

	var got int
	if err := db.QueryRow("SELECT COUNT(*) FROM webdav_locks WHERE token = ?", token).Scan(&got); err != nil {
		t.Fatalf("count lock rows: %v", err)
	}
	if got != want {
		t.Fatalf("lock row count for %q = %d, want %d", token, got, want)
	}
}

func assertNoLockResult(t *testing.T, result <-chan error, operation string) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("%s completed before guard release: %v", operation, err)
	case <-time.After(50 * time.Millisecond):
	}
}

func waitLockResult(t *testing.T, result <-chan error, operation string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not complete", operation)
		return nil
	}
}
