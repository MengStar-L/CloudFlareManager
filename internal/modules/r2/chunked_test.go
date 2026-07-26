package r2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func newChunkedTestService(t *testing.T, chunkBytes int64) (Service, *memoryBackend, string) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{6}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
		R2AccessKeyID: "access", R2SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	index := NewStore(db, Limits{StorageBytes: 1000, ClassA: 100, ClassB: 100})
	if _, err := index.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "physical"}); err != nil {
		t.Fatal(err)
	}
	backend := &memoryBackend{objects: map[string][]byte{}}
	tempDir := t.TempDir()
	return Service{
		Index: index, Accounts: accountStore, Backend: backend,
		TempDir: tempDir, ChunkBytes: chunkBytes,
	}, backend, tempDir
}

func stagedFileCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".r2-upload-") {
			count++
		}
	}
	return count
}

func TestPutChunksLargeUploads(t *testing.T) {
	t.Parallel()

	service, backend, tempDir := newChunkedTestService(t, 4)
	payload := "hello world!"
	digest := sha256.Sum256([]byte(payload))

	object, err := service.Put(context.Background(), PutRequest{
		Key: "big/object.bin", Body: strings.NewReader(payload), Size: int64(len(payload)),
		ContentType: "application/octet-stream", PayloadHash: hex.EncodeToString(digest[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	if object.Size != int64(len(payload)) || object.State != StateCommitted {
		t.Fatalf("object = %#v", object)
	}
	if object.ETag != "multipart-etag" {
		t.Fatalf("etag = %q, expected the multipart path", object.ETag)
	}
	result, err := service.Get(context.Background(), "big/object.bin", GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if string(data) != payload {
		t.Fatalf("round-trip body = %q", data)
	}
	if len(backend.uploads) != 0 {
		t.Fatalf("multipart upload not finalized: %#v", backend.uploads)
	}
	if count := stagedFileCount(t, tempDir); count != 0 {
		t.Fatalf("staged files left behind: %d", count)
	}

	// 未知长度（chunked transfer encoding）同样走分片路径。
	streamed, err := service.Put(context.Background(), PutRequest{
		Key: "big/streamed.bin", Body: strings.NewReader(payload), Size: -1,
		PayloadHash: "UNSIGNED-PAYLOAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if streamed.Size != int64(len(payload)) {
		t.Fatalf("streamed size = %d", streamed.Size)
	}
}

func TestPutChunkedAbortsOnPayloadHashMismatch(t *testing.T) {
	t.Parallel()

	service, backend, tempDir := newChunkedTestService(t, 4)
	wrong := sha256.Sum256([]byte("something else entirely"))

	_, err := service.Put(context.Background(), PutRequest{
		Key: "big/tampered.bin", Body: strings.NewReader("hello world!"), Size: 12,
		PayloadHash: hex.EncodeToString(wrong[:]),
	})
	if err != ErrPayloadHashMismatch {
		t.Fatalf("error = %v, want ErrPayloadHashMismatch", err)
	}
	if len(backend.uploads) != 0 {
		t.Fatalf("multipart upload should be aborted: %#v", backend.uploads)
	}
	if _, ok := backend.objects["physical/tampered.bin"]; ok {
		t.Fatal("aborted upload must not materialize an object")
	}
	if count := stagedFileCount(t, tempDir); count != 0 {
		t.Fatalf("staged files left behind: %d", count)
	}
}

func TestCleanupStagedUploads(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".r2-upload-orphan"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	CleanupStagedUploads(dir, nil)
	if _, err := os.Stat(filepath.Join(dir, ".r2-upload-orphan")); !os.IsNotExist(err) {
		t.Fatalf("orphan should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatalf("unrelated file must be preserved: %v", err)
	}
}
