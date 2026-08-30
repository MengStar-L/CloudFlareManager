package r2

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

func TestWebDAVNamespaceMigrationSettlesLegacyMultipartBeforeChangingKeys(t *testing.T) {
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()
	legacy, err := service.Put(ctx, PutRequest{Key: "legacy.txt", Body: strings.NewReader("old"), Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: "upload.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UploadPart(ctx, UploadPartRequest{
		Key: upload.Key, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("part"), Size: 4,
	}); err != nil {
		t.Fatal(err)
	}

	deferred, err := service.Index.EnsureWebDAVNamespaces(ctx, []string{"gamesync-id"})
	if err != nil || !deferred.Deferred {
		t.Fatalf("migration = %#v, error = %v", deferred, err)
	}
	currentUpload, err := service.Index.GetMultipart(ctx, upload.ID)
	if err != nil || currentUpload.Key != "upload.bin" {
		t.Fatalf("multipart changed before settlement: %#v, %v", currentUpload, err)
	}
	intent, err := service.Index.GetWriteIntent(ctx, upload.WriteIntentID)
	if err != nil || intent.Key != "upload.bin" {
		t.Fatalf("intent changed before settlement: %#v, %v", intent, err)
	}

	if err := service.PrepareWebDAVNamespaceMigration(ctx); err != nil {
		t.Fatal(err)
	}
	assertMultipartGone(t, service.Index, upload)
	migrated, err := service.Index.EnsureWebDAVNamespaces(ctx, []string{"gamesync-id"})
	if err != nil || migrated.Deferred || migrated.MigratedObjects != 1 {
		t.Fatalf("migration = %#v, error = %v", migrated, err)
	}
	if _, err := service.Stat(ctx, WebDAVMountPrefix("gamesync-id")+legacy.Key); err != nil {
		t.Fatalf("migrated object: %v", err)
	}
}

func TestOverwriteMigratedWebDAVObjectQueuesOldPhysicalKeyCleanup(t *testing.T) {
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()
	legacy, err := service.Put(ctx, PutRequest{Key: "save.dat", Body: strings.NewReader("old"), Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	bucket, err := service.Index.GetBucket(ctx, legacy.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.EnsureWebDAVNamespaces(ctx, []string{"gamesync-id"}); err != nil {
		t.Fatal(err)
	}
	key := WebDAVMountPrefix("gamesync-id") + legacy.Key
	updated, err := service.Put(ctx, PutRequest{Key: key, Body: strings.NewReader("new!"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if updated.PhysicalKey != key {
		t.Fatalf("new physical key = %q", updated.PhysicalKey)
	}
	cleanups, err := service.Index.ListPhysicalCleanups(ctx, 10)
	if err != nil || len(cleanups) != 1 {
		t.Fatalf("cleanups = %#v, error = %v", cleanups, err)
	}
	if cleanups[0].PhysicalKey != legacy.PhysicalKey || cleanups[0].BucketID != legacy.BucketID || cleanups[0].ExpectedETag != legacy.ETag {
		t.Fatalf("cleanup = %#v", cleanups[0])
	}
	bucket, err = service.Index.GetBucket(ctx, bucket.ID)
	if err != nil || bucket.StorageBytes != 7 {
		t.Fatalf("bucket before cleanup = %#v, error = %v", bucket, err)
	}
	completed, err := service.ProcessPhysicalCleanups(ctx, 10)
	if err != nil || completed != 1 {
		t.Fatalf("completed = %d, error = %v", completed, err)
	}
	bucket, err = service.Index.GetBucket(ctx, bucket.ID)
	if err != nil || bucket.StorageBytes != 4 {
		t.Fatalf("bucket after cleanup = %#v, error = %v", bucket, err)
	}
	backend := service.Backend.(*memoryBackend)
	if _, exists := backend.objects[bucket.Name+"/"+legacy.PhysicalKey]; exists {
		t.Fatal("legacy physical object remains after cleanup")
	}
}
