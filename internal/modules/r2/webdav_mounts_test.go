package r2

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestWebDAVMountPath(t *testing.T) {
	prefix := WebDAVMountPrefix("credential-id")
	if prefix != ".cf-r2-manager/webdav/credential-id/" {
		t.Fatalf("prefix = %q", prefix)
	}
	key, err := WebDAVMountKey("credential-id", "saves/slot.dat")
	if err != nil || key != prefix+"saves/slot.dat" {
		t.Fatalf("key = %q, err = %v", key, err)
	}
	visible, ok := WebDAVVisibleKey("credential-id", key)
	if !ok || visible != "saves/slot.dat" {
		t.Fatalf("visible = %q, ok = %v", visible, ok)
	}
}

func TestWebDAVMountKeyRejectsTraversal(t *testing.T) {
	for _, value := range []string{"../secret", "/absolute", `bad\key`, "a//b"} {
		if _, err := WebDAVMountKey("credential-id", value); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("value %q: err = %v", value, err)
		}
	}
}

func TestEnsureWebDAVNamespacesMigratesLegacyStateOnce(t *testing.T) {
	service, jobStore, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()
	for _, key := range []string{"GameSync/save.dat", "claude.png"} {
		if _, err := service.Put(ctx, PutRequest{Key: key, Size: 0}); err != nil {
			t.Fatal(err)
		}
	}
	db := service.Index.db
	if _, err := db.ExecContext(ctx, `INSERT INTO webdav_locks(token, object_key, owner, depth, expires_at, created_at)
		VALUES('token', 'GameSync/save.dat', '', 'infinity', ?, ?)`, time.Now().Add(time.Hour).Unix(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	job, err := jobStore.Enqueue(ctx, FileMoveJobType, FileJobPayload{Source: "GameSync/", Destination: "Archive/"}, 3)
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.Index.EnsureWebDAVNamespaces(ctx, []string{"gamesync-id", "test-id"})
	if err != nil {
		t.Fatal(err)
	}
	if result.TargetCredentialID != "gamesync-id" || result.MigratedObjects != 2 {
		t.Fatalf("migration = %#v", result)
	}
	for _, key := range []string{
		WebDAVMountPrefix("gamesync-id") + "GameSync/save.dat",
		WebDAVMountPrefix("gamesync-id") + "claude.png",
	} {
		if _, err := service.Stat(ctx, key); err != nil {
			t.Fatalf("migrated object %q: %v", key, err)
		}
	}
	var lockKey string
	if err := db.QueryRowContext(ctx, "SELECT object_key FROM webdav_locks WHERE token = 'token'").Scan(&lockKey); err != nil {
		t.Fatal(err)
	}
	if lockKey != WebDAVMountPrefix("gamesync-id")+"GameSync/save.dat" {
		t.Fatalf("lock key = %q", lockKey)
	}
	migratedJob, err := jobStore.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var payload FileJobPayload
	if err := json.Unmarshal(migratedJob.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Source != WebDAVMountPrefix("gamesync-id")+"GameSync/" || payload.Destination != WebDAVMountPrefix("gamesync-id")+"Archive/" {
		t.Fatalf("job payload = %#v", payload)
	}

	second, err := service.Index.EnsureWebDAVNamespaces(ctx, []string{"test-id"})
	if err != nil || !second.AlreadyComplete || second.TargetCredentialID != "gamesync-id" {
		t.Fatalf("second migration = %#v, %v", second, err)
	}
}

func TestEnsureWebDAVNamespacesDefersWithoutCredential(t *testing.T) {
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	if _, err := service.Put(context.Background(), PutRequest{Key: "legacy.txt", Size: 0}); err != nil {
		t.Fatal(err)
	}
	result, err := service.Index.EnsureWebDAVNamespaces(context.Background(), nil)
	if err != nil || !result.Deferred {
		t.Fatalf("migration = %#v, %v", result, err)
	}
}

func TestEnsureWebDAVNamespacesRejectsReservedCollision(t *testing.T) {
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := service.Put(ctx, PutRequest{Key: WebDAVNamespaceRoot + "existing/file.txt", Size: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.EnsureWebDAVNamespaces(ctx, []string{"gamesync-id"}); !errors.Is(err, ErrWebDAVNamespaceConflict) {
		t.Fatalf("err = %v", err)
	}
	if _, err := service.Stat(ctx, WebDAVNamespaceRoot+"existing/file.txt"); err != nil {
		t.Fatalf("collision object changed: %v", err)
	}
	var count int
	if err := service.Index.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM system_settings WHERE key = ?", webDAVNamespaceMigrationSetting).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("migration marker should not be written")
	}
}
