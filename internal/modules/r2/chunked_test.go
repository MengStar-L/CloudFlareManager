package r2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	bucket, err := index.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "physical"})
	if err != nil {
		t.Fatal(err)
	}
	if err := index.FinishBucketScan(context.Background(), bucket.ID, 0, false); err != nil {
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

func TestChunkedPutUsesPhysicalConditionsAtCompletion(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 4)
	ctx := context.Background()

	created, err := service.PutConditional(ctx, PutRequest{
		Key: "catalog.bin", Body: strings.NewReader("first-version"), Size: 13,
		Conditions: MutationConditions{IfNoneMatch: &EntityTagSet{Wildcard: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created || backend.completeOptions.IfNoneMatch != "*" || backend.completeOptions.IfMatch != "" {
		t.Fatalf("created = %#v, complete conditions = %#v", created, backend.completeOptions)
	}

	updated, err := service.PutConditional(ctx, PutRequest{
		Key: "catalog.bin", Body: strings.NewReader("second-version"), Size: 14,
		Conditions: MutationConditions{IfMatch: &EntityTagSet{Tags: []EntityTag{{Value: created.Object.ETag}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Created || backend.completeOptions.IfMatch != quoteETag(created.Object.ETag) || backend.completeOptions.IfNoneMatch != "" {
		t.Fatalf("updated = %#v, complete conditions = %#v", updated, backend.completeOptions)
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

type abortFailureBackend struct{ *memoryBackend }

func (b *abortFailureBackend) AbortMultipart(context.Context, Target, string, string) error {
	return errors.New("abort unavailable")
}

func TestUnknownLengthQuotaFailureKeepsReservationWhenAbortFails(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 2)
	service.Index.limits.StorageBytes = 3
	service.Index.limits.AccountStorageBytes = 3
	service.Backend = &abortFailureBackend{memoryBackend: backend}

	_, err := service.Put(context.Background(), PutRequest{
		Key: "stream.bin", Body: strings.NewReader("abcdef"), Size: -1, PayloadHash: "UNSIGNED-PAYLOAD",
	})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("error = %v, want quota exceeded", err)
	}
	intents, err := service.Index.ListWriteIntents(context.Background(), 10)
	if err != nil || len(intents) != 1 || intents[0].ReservedBytes != 2 || intents[0].State != WriteAborting {
		t.Fatalf("intents = %#v, error = %v", intents, err)
	}
	bucket, err := service.Index.GetBucket(context.Background(), intents[0].BucketID)
	if err != nil || bucket.ReservedBytes != 2 {
		t.Fatalf("bucket = %#v, error = %v", bucket, err)
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
