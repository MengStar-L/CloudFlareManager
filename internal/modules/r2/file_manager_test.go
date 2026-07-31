package r2

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestListDirectoryGroupsExplicitAndImplicitFolders(t *testing.T) {
	t.Parallel()
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()

	for _, item := range []struct {
		key, contentType string
		size             int64
	}{
		{key: "GameSync/", contentType: DirectoryContentType},
		{key: "GameSync/catalog/catalog.json", contentType: "application/json", size: 12},
		{key: "GameSync/readme.txt", contentType: "text/plain", size: 5},
		{key: "root.txt", contentType: "text/plain", size: 4},
	} {
		if _, err := service.Put(ctx, PutRequest{
			Key: item.key, Body: bytes.NewReader(make([]byte, item.size)), Size: item.size,
			ContentType: item.contentType, Metadata: directoryMetadata(item.key),
		}); err != nil {
			t.Fatal(err)
		}
	}

	root, err := service.ListDirectory(ctx, DirectoryListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if root.DirectoryCount != 1 || root.FileCount != 1 || len(root.Entries) != 2 {
		t.Fatalf("root listing = %#v", root)
	}
	if root.Entries[0].Kind != EntryDirectory || root.Entries[0].Key != "GameSync/" {
		t.Fatalf("first root entry = %#v", root.Entries[0])
	}
	if root.Entries[1].Kind != EntryFile || root.Entries[1].Key != "root.txt" {
		t.Fatalf("second root entry = %#v", root.Entries[1])
	}

	nested, err := service.ListDirectory(ctx, DirectoryListOptions{Path: "GameSync/", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if nested.DirectoryCount != 1 || nested.FileCount != 1 || len(nested.Entries) != 2 {
		t.Fatalf("nested listing = %#v", nested)
	}
	if nested.Entries[0].Key != "GameSync/catalog/" || nested.Entries[0].Kind != EntryDirectory {
		t.Fatalf("inferred directory = %#v", nested.Entries[0])
	}
}

func TestListDirectoryPaginatesChildrenInsteadOfObjects(t *testing.T) {
	t.Parallel()
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()
	for _, key := range []string{"alpha/1", "alpha/2", "beta/1", "root.txt"} {
		if _, err := service.Put(ctx, PutRequest{Key: key, Body: bytes.NewReader(nil), Size: 0}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := service.ListDirectory(ctx, DirectoryListOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 2 || first.NextMarker == "" || first.Entries[0].Key != "alpha/" || first.Entries[1].Key != "beta/" {
		t.Fatalf("first page = %#v", first)
	}
	second, err := service.ListDirectory(ctx, DirectoryListOptions{After: first.NextMarker, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Entries) != 1 || second.Entries[0].Key != "root.txt" || second.NextMarker != "" {
		t.Fatalf("second page = %#v", second)
	}
}

func TestListWebDAVDirectoryIsolatesMountsAndAllowsEmptyRoot(t *testing.T) {
	t.Parallel()
	service, _, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()
	for _, item := range []struct {
		mountID string
		key     string
	}{
		{mountID: "gamesync-id", key: "GameSync/save.dat"},
		{mountID: "gamesync-id", key: "claude.png"},
		{mountID: "test-id", key: "only-test.txt"},
	} {
		key, err := WebDAVMountKey(item.mountID, item.key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Put(ctx, PutRequest{Key: key, Size: 0}); err != nil {
			t.Fatal(err)
		}
	}

	gamesync, err := service.ListWebDAVDirectory(ctx, "gamesync-id", DirectoryListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if gamesync.DirectoryCount != 1 || gamesync.FileCount != 1 || len(gamesync.Entries) != 2 {
		t.Fatalf("gamesync listing = %#v", gamesync)
	}
	for _, entry := range gamesync.Entries {
		if entry.MountID != "gamesync-id" || IsWebDAVInternalKey(entry.Key) || entry.Key == "only-test.txt" {
			t.Fatalf("gamesync entry = %#v", entry)
		}
	}
	testMount, err := service.ListWebDAVDirectory(ctx, "test-id", DirectoryListOptions{Limit: 100})
	if err != nil || len(testMount.Entries) != 1 || testMount.Entries[0].Key != "only-test.txt" {
		t.Fatalf("test listing = %#v, %v", testMount, err)
	}
	empty, err := service.ListWebDAVDirectory(ctx, "empty-id", DirectoryListOptions{Limit: 100})
	if err != nil || len(empty.Entries) != 0 || empty.MountID != "empty-id" {
		t.Fatalf("empty listing = %#v, %v", empty, err)
	}
}

func TestFileJobsMoveMergesAndDeleteRecursively(t *testing.T) {
	t.Parallel()
	service, jobStore, cleanup := newFileManagerFixture(t)
	defer cleanup()
	ctx := context.Background()
	for key, value := range map[string]string{
		"source/a.txt": "new-a", "source/sub/b.txt": "new-b", "target/a.txt": "old-a", "target/keep.txt": "keep",
	} {
		if _, err := service.Put(ctx, PutRequest{Key: key, Body: bytes.NewBufferString(value), Size: int64(len(value)), ContentType: "text/plain"}); err != nil {
			t.Fatal(err)
		}
	}
	handler := FileJobs{Service: service, Jobs: jobStore}
	moveJob, err := jobStore.Enqueue(ctx, FileMoveJobType, FileJobPayload{
		Source: "source/", Destination: "target/", Overwrite: true,
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.Claim(ctx, 0)
	if err != nil || claimed == nil || claimed.ID != moveJob.ID {
		t.Fatalf("claimed move job = %#v, %v", claimed, err)
	}
	if err := handler.HandleMove(ctx, *claimed); err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Complete(ctx, moveJob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Stat(ctx, "source/a.txt"); err != ErrObjectNotFound {
		t.Fatalf("source still exists: %v", err)
	}
	for _, key := range []string{"target/a.txt", "target/sub/b.txt", "target/keep.txt"} {
		if _, err := service.Stat(ctx, key); err != nil {
			t.Fatalf("destination %s: %v", key, err)
		}
	}

	deleteJob, err := jobStore.Enqueue(ctx, FileDeleteJobType, FileJobPayload{Source: "target/"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = jobStore.Claim(ctx, 0)
	if err != nil || claimed == nil || claimed.ID != deleteJob.ID {
		t.Fatalf("claimed delete job = %#v, %v", claimed, err)
	}
	if err := handler.HandleDelete(ctx, *claimed); err != nil {
		t.Fatal(err)
	}
	remaining, err := service.List(ctx, ListOptions{Prefix: "target/", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Objects) != 0 {
		t.Fatalf("remaining target objects = %#v", remaining.Objects)
	}
}

func newFileManagerFixture(t *testing.T) (Service, *jobs.Store, func()) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{15}, secret.KeySize))
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
	index := NewStore(db, Limits{StorageBytes: 1 << 30, ClassA: 10000, ClassB: 10000})
	bucket, err := index.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "physical"})
	if err != nil {
		t.Fatal(err)
	}
	if err := index.FinishBucketScan(context.Background(), bucket.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	service := Service{Index: index, Accounts: accountStore, Backend: &memoryBackend{objects: map[string][]byte{}}, TempDir: t.TempDir()}
	return service, jobs.NewStore(db), func() { _ = db.Close() }
}

func directoryMetadata(key string) map[string]string {
	if len(key) > 0 && key[len(key)-1] == '/' {
		return map[string]string{"webdav-directory": "true"}
	}
	return nil
}
