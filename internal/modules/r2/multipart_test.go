package r2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestServiceMultipartUploadPersistsPartsAndCommitsObject(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{9}, secret.KeySize))
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
	service := Service{Index: index, Accounts: accountStore, Backend: backend, TempDir: t.TempDir()}

	upload, err := service.CreateMultipart(context.Background(), CreateMultipartInput{
		Key: "large.bin", ContentType: "application/octet-stream", Metadata: map[string]string{"origin": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.UploadPart(context.Background(), UploadPartRequest{
		Key: upload.Key, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("hello "), Size: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.UploadPart(context.Background(), UploadPartRequest{
		Key: upload.Key, UploadID: upload.ID, PartNumber: 2, Body: strings.NewReader("world"), Size: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := service.ListParts(context.Background(), upload.Key, upload.ID, 0, 1000)
	if err != nil || len(listed.Parts) != 2 {
		t.Fatalf("parts = %#v, error = %v", listed, err)
	}
	object, err := service.CompleteMultipart(context.Background(), CompleteMultipartRequest{
		Key: upload.Key, UploadID: upload.ID,
		Parts: []CompletedPart{{PartNumber: 1, ETag: first.ETag}, {PartNumber: 2, ETag: second.ETag}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if object.Size != 11 || object.ETag != "multipart-etag" || object.Metadata["origin"] != "test" {
		t.Fatalf("object = %#v", object)
	}
	result, err := service.Get(context.Background(), object.Key, GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if string(data) != "hello world" {
		t.Fatalf("body = %q", data)
	}
	if _, err := index.GetMultipart(context.Background(), upload.ID); !errors.Is(err, ErrMultipartNotFound) {
		t.Fatalf("completed upload error = %v", err)
	}
}

func TestMultipartPartReplacementReservesOnlySizeDifference(t *testing.T) {
	service, _, _ := newChunkedTestService(t, 64)
	upload, err := service.CreateMultipart(context.Background(), CreateMultipartInput{Key: "replace.bin"})
	if err != nil {
		t.Fatal(err)
	}
	part, err := service.UploadPart(context.Background(), UploadPartRequest{
		Key: upload.Key, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("123456"), Size: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertReserved := func(want int64) {
		t.Helper()
		bucket, err := service.Index.GetBucket(context.Background(), upload.BucketID)
		if err != nil || bucket.ReservedBytes != want {
			t.Fatalf("reserved bytes = %d, want %d (error %v)", bucket.ReservedBytes, want, err)
		}
	}
	assertReserved(6)
	part, err = service.UploadPart(context.Background(), UploadPartRequest{
		Key: upload.Key, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("1234567890"), Size: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertReserved(10)
	part, err = service.UploadPart(context.Background(), UploadPartRequest{
		Key: upload.Key, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("1234"), Size: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertReserved(4)
	object, err := service.CompleteMultipart(context.Background(), CompleteMultipartRequest{
		Key: upload.Key, UploadID: upload.ID, Parts: []CompletedPart{{PartNumber: 1, ETag: part.ETag}},
	})
	if err != nil || object.Size != 4 {
		t.Fatalf("object = %#v, error = %v", object, err)
	}
	assertReserved(0)
}

func TestMultipartExpiresAfterTwentyFourHoursAndReleasesReservation(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	upload, err := service.CreateMultipart(context.Background(), CreateMultipartInput{Key: "expired.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UploadPart(context.Background(), UploadPartRequest{
		Key: upload.Key, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("data"), Size: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(context.Background(), `UPDATE r2_multipart_uploads SET updated_at = ? WHERE id = ?`,
		time.Now().Add(-25*time.Hour).UnixNano(), upload.ID); err != nil {
		t.Fatal(err)
	}
	expired, err := service.ExpireMultipart(context.Background(), 24*time.Hour, 10)
	if err != nil || expired != 1 {
		t.Fatalf("expired = %d, error = %v", expired, err)
	}
	if _, err := service.Index.GetMultipart(context.Background(), upload.ID); !errors.Is(err, ErrMultipartNotFound) {
		t.Fatalf("multipart remains: %v", err)
	}
	if len(backend.uploads) != 0 {
		t.Fatalf("upstream multipart remains: %#v", backend.uploads)
	}
	bucket, _ := service.Index.GetBucket(context.Background(), upload.BucketID)
	if bucket.ReservedBytes != 0 {
		t.Fatalf("reserved bytes after expiry = %d", bucket.ReservedBytes)
	}
}

func TestExpiredCompletingMultipartCommitsMatchingRemoteObject(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: "completed-remotely.bin"})
	if err != nil {
		t.Fatal(err)
	}
	part, err := service.UploadPart(ctx, UploadPartRequest{
		Key: upload.Key, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("data"), Size: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Index.BeginCompleteMultipart(ctx, upload.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.MarkWriteCompleting(ctx, upload.WriteIntentID, "", 4); err != nil {
		t.Fatal(err)
	}
	target, err := service.target(ctx, upload.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CompleteMultipart(ctx, target, upload.Key, upload.UpstreamID,
		[]CompletedPart{{PartNumber: 1, ETag: part.ETag}}); err != nil {
		t.Fatal(err)
	}
	ageMultipartUpload(t, service.Index, upload.ID)

	expired, err := service.ExpireMultipart(ctx, 24*time.Hour, 10)
	if err != nil || expired != 1 {
		t.Fatalf("expired = %d, error = %v", expired, err)
	}
	object, err := service.Index.GetObject(ctx, upload.Key)
	if err != nil || object.Size != 4 || object.ETag != "multipart-etag" {
		t.Fatalf("object = %#v, error = %v", object, err)
	}
	if _, err := service.Index.GetMultipart(ctx, upload.ID); !errors.Is(err, ErrMultipartNotFound) {
		t.Fatalf("multipart remains: %v", err)
	}
	bucket, _ := service.Index.GetBucket(ctx, upload.BucketID)
	if bucket.ReservedBytes != 0 || bucket.StorageBytes != 4 {
		t.Fatalf("bucket after recovered completion = %#v", bucket)
	}
}

func TestExpiredCompletingMultipartAbortsWhenPriorObjectStillExists(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	previous, err := service.Put(ctx, PutRequest{Key: "overwrite.bin", Size: 3, Body: strings.NewReader("old")})
	if err != nil {
		t.Fatal(err)
	}
	upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: previous.Key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UploadPart(ctx, UploadPartRequest{
		Key: upload.Key, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("new-data"), Size: 8,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.BeginCompleteMultipart(ctx, upload.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.MarkWriteCompleting(ctx, upload.WriteIntentID, "", 8); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, `UPDATE r2_write_intents SET operation = ? WHERE id = ?`,
		WriteOperationLegacyMultipart, upload.WriteIntentID); err != nil {
		t.Fatal(err)
	}
	ageMultipartUpload(t, service.Index, upload.ID)

	expired, err := service.ExpireMultipart(ctx, 24*time.Hour, 10)
	if err != nil || expired != 1 {
		t.Fatalf("expired = %d, error = %v", expired, err)
	}
	current, err := service.Index.GetObject(ctx, previous.Key)
	if err != nil || current.ObjectID != previous.ObjectID {
		t.Fatalf("committed object changed: %#v, error = %v", current, err)
	}
	if len(backend.uploads) != 0 {
		t.Fatalf("upstream multipart remains: %#v", backend.uploads)
	}
	bucket, _ := service.Index.GetBucket(ctx, upload.BucketID)
	if bucket.ReservedBytes != 0 || bucket.StorageBytes != previous.Size {
		t.Fatalf("bucket after aborted completion = %#v", bucket)
	}
}

func TestExpiredCompletingMultipartKeepsReservationWhenHeadIsUncertain(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: "uncertain.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UploadPart(ctx, UploadPartRequest{
		Key: upload.Key, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("data"), Size: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.BeginCompleteMultipart(ctx, upload.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.MarkWriteCompleting(ctx, upload.WriteIntentID, "", 4); err != nil {
		t.Fatal(err)
	}
	ageMultipartUpload(t, service.Index, upload.ID)
	service.Backend = &headFailureBackend{memoryBackend: backend}

	if expired, err := service.ExpireMultipart(ctx, 24*time.Hour, 10); err == nil || expired != 0 {
		t.Fatalf("expired = %d, error = %v", expired, err)
	}
	if _, err := service.Index.GetMultipart(ctx, upload.ID); err != nil {
		t.Fatalf("multipart should remain: %v", err)
	}
	bucket, _ := service.Index.GetBucket(ctx, upload.BucketID)
	if bucket.ReservedBytes != 4 {
		t.Fatalf("reserved bytes after uncertain HEAD = %d", bucket.ReservedBytes)
	}
	if len(backend.uploads) != 1 {
		t.Fatalf("upstream multipart should remain: %#v", backend.uploads)
	}
}

type headFailureBackend struct {
	*memoryBackend
}

func (b *headFailureBackend) Head(context.Context, Target, string) (RemoteObject, error) {
	return RemoteObject{}, errors.New("temporary HEAD failure")
}

func ageMultipartUpload(t *testing.T, store *Store, uploadID string) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `UPDATE r2_multipart_uploads SET updated_at = ? WHERE id = ?`,
		time.Now().Add(-25*time.Hour).UnixNano(), uploadID); err != nil {
		t.Fatal(err)
	}
}
