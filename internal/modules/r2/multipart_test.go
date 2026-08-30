package r2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sort"
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

func TestCreateMultipartDefinitiveFailureReleasesWriteIntent(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "conditional conflict", err: ErrConditionalRequestConflict},
		{name: "rate limited", err: ErrRateLimited},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, backend, _ := newChunkedTestService(t, 64)
			ctx := context.Background()
			backend.createError = test.err

			if _, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: "retry.bin"}); !errors.Is(err, test.err) {
				t.Fatalf("CreateMultipart error = %v", err)
			}
			intents, err := service.Index.ListWriteIntents(ctx, 10)
			if err != nil || len(intents) != 0 {
				t.Fatalf("write intents after definitive rejection = %#v, error = %v", intents, err)
			}
			var uploads int
			if err := service.Index.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM r2_multipart_uploads`).Scan(&uploads); err != nil || uploads != 0 {
				t.Fatalf("multipart rows after definitive rejection = %d, error = %v", uploads, err)
			}

			backend.createError = nil
			upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: "retry.bin"})
			if err != nil {
				t.Fatalf("retry CreateMultipart: %v", err)
			}
			if err := service.AbortMultipart(ctx, upload.Key, upload.ID); err != nil {
				t.Fatalf("abort retry upload: %v", err)
			}
		})
	}
}

func TestCreateMultipartBackfillsMissingETagBeforeWriteIntent(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	previous, err := service.Put(ctx, PutRequest{Key: "empty-etag.bin", Body: strings.NewReader("old"), Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, `UPDATE r2_objects SET etag = '' WHERE object_id = ?`, previous.ObjectID); err != nil {
		t.Fatal(err)
	}
	upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: previous.Key})
	if err != nil {
		t.Fatal(err)
	}
	repaired, err := service.Index.GetObject(ctx, previous.Key)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.ETag != previous.ETag || backend.headCalls != 1 {
		t.Fatalf("repaired object = %#v, HEAD calls = %d", repaired, backend.headCalls)
	}
	part, err := service.UploadPart(ctx, UploadPartRequest{
		Key: upload.Key, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("new"), Size: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := CompleteMultipartRequest{
		Key: upload.Key, UploadID: upload.ID, Parts: []CompletedPart{{PartNumber: 1, ETag: part.ETag}},
	}
	completed, err := service.CompleteMultipart(ctx, request)
	if err != nil {
		t.Fatalf("CompleteMultipart: %v", err)
	}
	if completed.ETag == "" {
		t.Fatalf("completed object has no ETag: %#v", completed)
	}
}

func TestCompleteMultipartConditionalConflictReleasesConsumedUpload(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	upload, request := createMultipartCompletionRequest(t, service, "conditional-conflict.bin")
	backend.completeError = ErrConditionalRequestConflict
	if _, err := service.CompleteMultipart(ctx, request); !errors.Is(err, ErrConditionalRequestConflict) {
		t.Fatalf("CompleteMultipart error = %v", err)
	}
	assertMultipartGone(t, service.Index, upload)
	if len(backend.uploads) != 0 {
		t.Fatalf("consumed upstream upload remains: %#v", backend.uploads)
	}
	backend.completeError = nil
	replacement, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: upload.Key})
	if err != nil {
		t.Fatalf("key was not reusable after conflict: %v", err)
	}
	if err := service.AbortMultipart(ctx, replacement.Key, replacement.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteMultipartRateLimitRemainsRetryable(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	upload, request := createMultipartCompletionRequest(t, service, "retry-complete.bin")
	backend.completeError = ErrRateLimited
	if _, err := service.CompleteMultipart(ctx, request); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("CompleteMultipart error = %v", err)
	}
	assertMultipartRetryable(t, service.Index, upload)
	backend.completeError = nil
	if _, err := service.CompleteMultipart(ctx, request); err != nil {
		t.Fatalf("retry CompleteMultipart: %v", err)
	}
}

func TestCompleteMultipartNoSuchUploadDoesNotReviveConsumedID(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	upload, request := createMultipartCompletionRequest(t, service, "missing-upload.bin")
	backend.completeError = testRemoteError{code: "NoSuchUpload", status: 404}
	if _, err := service.CompleteMultipart(ctx, request); !isMultipartNotFound(err) {
		t.Fatalf("CompleteMultipart error = %v", err)
	}
	assertMultipartGone(t, service.Index, upload)
}

type publishOnAbortBackend struct {
	*memoryBackend
}

func (b *publishOnAbortBackend) CompleteMultipart(
	_ context.Context,
	_ Target,
	_ string,
	_ string,
	_ []CompletedPart,
	options PutOptions,
) (string, error) {
	b.completeOptions = options
	return "", testRemoteError{code: "NoSuchUpload", status: 404}
}

func (b *publishOnAbortBackend) AbortMultipart(_ context.Context, target Target, key, uploadID string) error {
	parts := b.uploads[uploadID]
	partNumbers := make([]int, 0, len(parts))
	for number := range parts {
		partNumbers = append(partNumbers, int(number))
	}
	sort.Ints(partNumbers)
	var data []byte
	for _, number := range partNumbers {
		data = append(data, parts[int32(number)]...)
	}
	physicalKey := target.Bucket + "/" + key
	b.objects[physicalKey] = data
	if b.metadata == nil {
		b.metadata = make(map[string]map[string]string)
	}
	b.metadata[physicalKey] = cloneMetadata(b.uploadMetadata[uploadID])
	if b.etags == nil {
		b.etags = make(map[string]string)
	}
	b.etags[physicalKey] = "published-during-abort"
	delete(b.uploads, uploadID)
	delete(b.targets, uploadID)
	delete(b.uploadMetadata, uploadID)
	return testRemoteError{code: "NoSuchUpload", status: 404}
}

func TestConsumedMultipartCommitsObjectPublishedDuringAbort(t *testing.T) {
	service, memory, _ := newChunkedTestService(t, 64)
	backend := &publishOnAbortBackend{memoryBackend: memory}
	service.Backend = backend
	upload, request := createMultipartCompletionRequest(t, service, "abort-race.bin")

	object, err := service.CompleteMultipart(context.Background(), request)
	if err != nil {
		t.Fatalf("CompleteMultipart: %v", err)
	}
	if object.ObjectID != upload.WriteIntentID || object.Size != 4 || object.ETag != "published-during-abort" {
		t.Fatalf("committed object = %#v", object)
	}
	assertMultipartGone(t, service.Index, upload)
}

func TestInternalMultipartCommitsObjectPublishedDuringAbort(t *testing.T) {
	service, memory, _ := newChunkedTestService(t, 4)
	backend := &publishOnAbortBackend{memoryBackend: memory}
	service.Backend = backend

	result, err := service.Put(context.Background(), PutRequest{
		Key: "internal-abort-race.bin", Body: strings.NewReader("abcdefgh"), Size: 8,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if result.Size != 8 || result.ETag != "published-during-abort" {
		t.Fatalf("committed object = %#v", result)
	}
}

func TestAbortMultipartCommitsObjectPublishedDuringAbort(t *testing.T) {
	service, memory, _ := newChunkedTestService(t, 64)
	backend := &publishOnAbortBackend{memoryBackend: memory}
	service.Backend = backend
	upload, _ := createMultipartCompletionRequest(t, service, "explicit-abort-race.bin")

	if err := service.AbortMultipart(context.Background(), upload.Key, upload.ID); err != nil {
		t.Fatalf("AbortMultipart: %v", err)
	}
	object, err := service.Index.GetObject(context.Background(), upload.Key)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if object.ObjectID != upload.WriteIntentID || object.Size != 4 || object.ETag != "published-during-abort" {
		t.Fatalf("committed object = %#v", object)
	}
	bucket, err := service.Index.GetBucket(context.Background(), upload.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	if bucket.ReservedBytes != 0 || bucket.StorageBytes != 4 {
		t.Fatalf("bucket after abort race = %#v", bucket)
	}
}

func TestWriteIntentRemoteClassifierIsFailSafe(t *testing.T) {
	service, _, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	previous, err := service.Put(ctx, PutRequest{Key: "same-version.bin", Body: strings.NewReader("data"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := service.Index.BeginWrite(ctx, BeginWriteInput{ObjectInput: ObjectInput{Key: previous.Key, Size: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Index.MarkWriteCompleting(ctx, intent.ID, previous.ETag, previous.Size); err != nil {
		t.Fatal(err)
	}
	intent, err = service.Index.GetWriteIntent(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}

	state, err := service.classifyWriteIntentRemote(ctx, intent, &RemoteObject{
		Key: intent.Key, Size: previous.Size, ETag: previous.ETag,
	})
	if err != nil || state != remoteWriteAmbiguous {
		t.Fatalf("same intent/previous signature state = %v, error = %v", state, err)
	}
	state, err = service.classifyWriteIntentRemote(ctx, intent, nil)
	if err != nil || state != remoteWriteAmbiguous {
		t.Fatalf("missing previous physical object state = %v, error = %v", state, err)
	}
}

func TestAbortMultipartTreatsNoSuchUploadAsSuccess(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	upload, _ := createMultipartCompletionRequest(t, service, "already-aborted.bin")
	backend.abortError = testRemoteError{code: "NoSuchUpload", status: 404}
	if err := service.AbortMultipart(ctx, upload.Key, upload.ID); err != nil {
		t.Fatalf("AbortMultipart: %v", err)
	}
	assertMultipartGone(t, service.Index, upload)
}

type blockingCompleteBackend struct {
	*memoryBackend
	started chan struct{}
	release chan struct{}
}

func (b *blockingCompleteBackend) CompleteMultipart(
	ctx context.Context,
	target Target,
	key string,
	uploadID string,
	parts []CompletedPart,
	options PutOptions,
) (string, error) {
	close(b.started)
	select {
	case <-b.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return b.memoryBackend.CompleteMultipart(ctx, target, key, uploadID, parts, options)
}

func TestRecoveryDoesNotResetActiveMultipartCompletion(t *testing.T) {
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	upload, request := createMultipartCompletionRequest(t, service, "live-complete.bin")
	blocking := &blockingCompleteBackend{
		memoryBackend: backend,
		started:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	service.Backend = blocking
	type completionResult struct {
		object Object
		err    error
	}
	result := make(chan completionResult, 1)
	go func() {
		object, err := service.CompleteMultipart(ctx, request)
		result <- completionResult{object: object, err: err}
	}()
	<-blocking.started

	if err := service.RecoverInterrupted(ctx); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	current, err := service.Index.GetMultipart(ctx, upload.ID)
	if err != nil || current.Status != MultipartCompleting {
		t.Fatalf("multipart changed during live completion: %#v, %v", current, err)
	}
	intent, err := service.Index.GetWriteIntent(ctx, upload.WriteIntentID)
	if err != nil || intent.State != WriteCompleting {
		t.Fatalf("intent changed during live completion: %#v, %v", intent, err)
	}

	close(blocking.release)
	completed := <-result
	if completed.err != nil || completed.object.Key != upload.Key {
		t.Fatalf("CompleteMultipart = %#v, %v", completed.object, completed.err)
	}
}

func TestResetMultipartRollsBackBothStatesOnFailure(t *testing.T) {
	service, _, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: "atomic-reset.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Index.BeginCompleteMultipart(ctx, upload.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.MarkWriteCompleting(ctx, upload.WriteIntentID, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, `CREATE TRIGGER fail_write_intent_reset
		BEFORE UPDATE OF state ON r2_write_intents
		WHEN OLD.state = 'completing' AND NEW.state = 'uploading'
		BEGIN SELECT RAISE(ABORT, 'forced write intent reset failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.ResetMultipart(ctx, upload.ID); err == nil {
		t.Fatal("ResetMultipart succeeded despite forced write intent failure")
	}
	current, err := service.Index.GetMultipart(ctx, upload.ID)
	if err != nil || current.Status != MultipartCompleting {
		t.Fatalf("multipart after rolled-back reset = %#v, error = %v", current, err)
	}
	intent, err := service.Index.GetWriteIntent(ctx, upload.WriteIntentID)
	if err != nil || intent.State != WriteCompleting {
		t.Fatalf("write intent after rolled-back reset = %#v, error = %v", intent, err)
	}
	if _, err := service.Index.db.ExecContext(ctx, `DROP TRIGGER fail_write_intent_reset`); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.ResetMultipart(ctx, upload.ID); err != nil {
		t.Fatal(err)
	}
	assertMultipartRetryable(t, service.Index, upload)
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
		[]CompletedPart{{PartNumber: 1, ETag: part.ETag}}, PutOptions{IfNoneMatch: "*"}); err != nil {
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

func TestMultipartExpiryDefersFailureAndContinuesBatch(t *testing.T) {
	service, memory, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	blocked, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: "blocked-expiry.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UploadPart(ctx, UploadPartRequest{
		Key: blocked.Key, UploadID: blocked.ID, PartNumber: 1, Body: strings.NewReader("data"), Size: 4,
	}); err != nil {
		t.Fatal(err)
	}
	healthy, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: "healthy-expiry.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UploadPart(ctx, UploadPartRequest{
		Key: healthy.Key, UploadID: healthy.ID, PartNumber: 1, Body: strings.NewReader("more"), Size: 4,
	}); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-25 * time.Hour)
	if _, err := service.Index.db.ExecContext(ctx, `UPDATE r2_multipart_uploads SET updated_at = ? WHERE id = ?`,
		aged.UnixNano(), blocked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, `UPDATE r2_multipart_uploads SET updated_at = ? WHERE id = ?`,
		aged.Add(time.Second).UnixNano(), healthy.ID); err != nil {
		t.Fatal(err)
	}
	service.Backend = &headFailureForKeyBackend{memoryBackend: memory, key: blocked.Key}
	retryFloor := time.Now().Add(-time.Minute)

	expired, err := service.ExpireMultipart(ctx, 24*time.Hour, 10)
	if err == nil || expired != 1 {
		t.Fatalf("expired = %d, error = %v", expired, err)
	}
	deferred, err := service.Index.GetMultipart(ctx, blocked.ID)
	if err != nil {
		t.Fatalf("blocked multipart should remain: %v", err)
	}
	if deferred.LastModified.Before(retryFloor) {
		t.Fatalf("blocked multipart retry was not deferred: %v", deferred.LastModified)
	}
	if _, err := service.Index.GetMultipart(ctx, healthy.ID); !errors.Is(err, ErrMultipartNotFound) {
		t.Fatalf("healthy expired multipart remains: %v", err)
	}
	bucket, err := service.Index.GetBucket(ctx, blocked.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	if bucket.ReservedBytes != 4 {
		t.Fatalf("reserved bytes after partial expiry = %d", bucket.ReservedBytes)
	}
}

type headFailureBackend struct {
	*memoryBackend
}

func (b *headFailureBackend) Head(context.Context, Target, string) (RemoteObject, error) {
	return RemoteObject{}, errors.New("temporary HEAD failure")
}

type headFailureForKeyBackend struct {
	*memoryBackend
	key string
}

func (b *headFailureForKeyBackend) Head(ctx context.Context, target Target, key string) (RemoteObject, error) {
	if key == b.key {
		return RemoteObject{}, errors.New("temporary HEAD failure")
	}
	return b.memoryBackend.Head(ctx, target, key)
}

func ageMultipartUpload(t *testing.T, store *Store, uploadID string) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `UPDATE r2_multipart_uploads SET updated_at = ? WHERE id = ?`,
		time.Now().Add(-25*time.Hour).UnixNano(), uploadID); err != nil {
		t.Fatal(err)
	}
}

func assertMultipartRetryable(t *testing.T, store *Store, upload MultipartUpload) {
	t.Helper()
	current, err := store.GetMultipart(context.Background(), upload.ID)
	if err != nil || current.Status != MultipartActive {
		t.Fatalf("multipart state = %#v, error = %v", current, err)
	}
	intent, err := store.GetWriteIntent(context.Background(), upload.WriteIntentID)
	if err != nil || intent.State != WriteUploading {
		t.Fatalf("write intent state = %#v, error = %v", intent, err)
	}
}

func createMultipartCompletionRequest(t *testing.T, service Service, key string) (MultipartUpload, CompleteMultipartRequest) {
	t.Helper()
	ctx := context.Background()
	upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	part, err := service.UploadPart(ctx, UploadPartRequest{
		Key: upload.Key, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("data"), Size: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	return upload, CompleteMultipartRequest{
		Key: upload.Key, UploadID: upload.ID,
		Parts: []CompletedPart{{PartNumber: 1, ETag: part.ETag}},
	}
}

func assertMultipartGone(t *testing.T, store *Store, upload MultipartUpload) {
	t.Helper()
	ctx := context.Background()
	if current, err := store.GetMultipart(ctx, upload.ID); !errors.Is(err, ErrMultipartNotFound) {
		t.Fatalf("multipart still exists: %#v, error = %v", current, err)
	}
	if intent, err := store.GetWriteIntent(ctx, upload.WriteIntentID); !errors.Is(err, ErrWriteIntentNotFound) {
		t.Fatalf("write intent still exists: %#v, error = %v", intent, err)
	}
	bucket, err := store.GetBucket(ctx, upload.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	if bucket.ReservedBytes != 0 {
		t.Fatalf("reserved bytes = %d, want 0", bucket.ReservedBytes)
	}
}
