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

func TestServicePutGetDelete(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{5}, secret.KeySize))
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

	object, err := service.Put(context.Background(), PutRequest{
		Key: "docs/readme.txt", Body: strings.NewReader("hello"), Size: 5,
		ContentType: "text/plain", PayloadHash: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		Metadata: map[string]string{"user": "visible", "CF-R2-Manager-Write-ID": "forged"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if object.State != StateCommitted {
		t.Fatalf("object state = %q", object.State)
	}
	if object.Metadata["user"] != "visible" || object.Metadata["CF-R2-Manager-Write-ID"] != "" {
		t.Fatalf("committed metadata = %#v", object.Metadata)
	}
	result, err := service.Get(context.Background(), "docs/readme.txt", GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if string(data) != "hello" {
		t.Fatalf("body = %q", data)
	}
	if result.Metadata["user"] != "visible" || result.Metadata[InternalWriteIDMetadata] != "" {
		t.Fatalf("visible metadata = %#v", result.Metadata)
	}
	if err := service.Delete(context.Background(), "docs/readme.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := index.GetObject(context.Background(), "docs/readme.txt"); err != ErrObjectNotFound {
		t.Fatalf("deleted object error = %v", err)
	}
}

func TestAccountCredentialUpdatePreservesMappedTarget(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{10}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	ctx := context.Background()
	account, err := accountStore.Create(ctx, accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "old-token",
		R2AccessKeyID: "old-access", R2SecretAccessKey: "old-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	index := NewStore(db, Limits{StorageBytes: 1000, ClassA: 100, ClassB: 100})
	bucket, err := index.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: "physical"})
	if err != nil {
		t.Fatal(err)
	}
	object, err := index.ReservePut(ctx, ObjectInput{Key: "mapped.txt", Size: 6, ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if err := index.CommitPut(ctx, object.ObjectID, "etag", 6); err != nil {
		t.Fatal(err)
	}
	service := Service{Index: index, Accounts: accountStore}
	before, err := service.target(ctx, bucket.ID)
	if err != nil {
		t.Fatal(err)
	}

	newToken, newAccess, newSecret := "new-token", "new-access", "new-secret"
	if _, err := accountStore.UpdateCredentials(ctx, account.ID, accounts.UpdateCredentialsInput{
		APIToken: &newToken, R2AccessKeyID: &newAccess, R2SecretAccessKey: &newSecret,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := service.target(ctx, bucket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.AccountID != before.AccountID || after.CloudflareAccountID != before.CloudflareAccountID ||
		after.Bucket != before.Bucket || after.AccessKeyID != newAccess || after.SecretAccessKey != newSecret {
		t.Fatalf("target changed identity instead of credentials: before=%#v after=%#v", before, after)
	}
	mapped, err := index.GetObject(ctx, object.Key)
	if err != nil {
		t.Fatal(err)
	}
	if mapped.ObjectID != object.ObjectID || mapped.BucketID != bucket.ID || mapped.PhysicalKey != object.PhysicalKey {
		t.Fatalf("object mapping changed during credential update: before=%#v after=%#v", object, mapped)
	}
}

func TestServiceBackfillsMissingETagAndFencesPhysicalReads(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	object, err := service.Put(ctx, PutRequest{Key: "legacy.txt", Body: strings.NewReader("data"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	physicalKey := "physical/" + object.PhysicalKey
	backend.etags[physicalKey] = "repaired-etag"
	clearETag := func() {
		t.Helper()
		if _, err := service.Index.db.ExecContext(ctx, "UPDATE r2_objects SET etag = '' WHERE object_id = ?", object.ObjectID); err != nil {
			t.Fatal(err)
		}
	}

	clearETag()
	repaired, err := service.Stat(ctx, object.Key)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.ETag != "repaired-etag" || backend.headCalls != 1 {
		t.Fatalf("repaired object = %#v, HEAD calls = %d", repaired, backend.headCalls)
	}
	result, err := service.Get(ctx, object.Key, GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = result.Body.Close()
	if backend.getOptions.IfMatch != `"repaired-etag"` {
		t.Fatalf("physical GET conditions = %#v", backend.getOptions)
	}
	if backend.headCalls != 1 {
		t.Fatalf("already repaired object caused another HEAD: %d", backend.headCalls)
	}

	clearETag()
	copied, err := service.CopyConditional(ctx, object.Key, "copy.txt", MutationConditions{})
	if err != nil {
		t.Fatal(err)
	}
	if copied.Object.ETag == "" || backend.getOptions.IfMatch != `"repaired-etag"` {
		t.Fatalf("copy = %#v, physical GET conditions = %#v", copied, backend.getOptions)
	}

	clearETag()
	if _, err := service.PutConditional(ctx, PutRequest{Key: object.Key, Body: strings.NewReader("next"), Size: 4}); err != nil {
		t.Fatal(err)
	}
	if backend.putOptions.IfMatch != `"repaired-etag"` {
		t.Fatalf("replacement physical conditions = %#v", backend.putOptions)
	}
}

func TestServiceGetRejectsReplacedLogicalVersionWithSameETag(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	first, err := service.Put(ctx, PutRequest{Key: "versioned.txt", Body: strings.NewReader("same"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Put(ctx, PutRequest{Key: "versioned.txt", Body: strings.NewReader("same"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if first.ObjectID == second.ObjectID || first.ETag != second.ETag {
		t.Fatalf("logical versions = %#v then %#v", first, second)
	}
	backend.getCalls = 0
	if _, err := service.Get(ctx, first.Key, GetOptions{
		IfMatch: quoteETag(first.ETag), ExpectedObjectID: first.ObjectID,
	}); !errors.Is(err, ErrConditionalRequestConflict) {
		t.Fatalf("stale logical GET error = %v", err)
	}
	if backend.getCalls != 0 {
		t.Fatalf("physical GET calls after stale logical version = %d", backend.getCalls)
	}

	result, err := service.Get(ctx, second.Key, GetOptions{ExpectedObjectID: second.ObjectID})
	if err != nil {
		t.Fatal(err)
	}
	_ = result.Body.Close()
	if backend.getOptions.ExpectedObjectID != "" {
		t.Fatalf("logical version leaked to backend options: %#v", backend.getOptions)
	}
}

func TestServiceGetHoldsKeyActivityUntilBackendResponseEstablished(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	object, err := service.Put(ctx, PutRequest{Key: "guarded.txt", Body: strings.NewReader("data"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingGetBackend{
		memoryBackend: backend, started: make(chan struct{}), release: make(chan struct{}),
	}
	service.Backend = blocking
	resultChannel := make(chan struct {
		result GetResult
		err    error
	}, 1)
	go func() {
		result, getErr := service.Get(ctx, object.Key, GetOptions{ExpectedObjectID: object.ObjectID})
		resultChannel <- struct {
			result GetResult
			err    error
		}{result: result, err: getErr}
	}()
	<-blocking.started
	if finish, acquired := service.Index.tryBeginWriteActivity(object.Key); acquired {
		finish()
		t.Fatal("write activity acquired while versioned backend GET was being established")
	}
	close(blocking.release)
	read := <-resultChannel
	if read.err != nil {
		t.Fatal(read.err)
	}
	defer read.result.Body.Close()
	finish, acquired := service.Index.tryBeginWriteActivity(object.Key)
	if !acquired {
		t.Fatal("write activity remained locked after backend GET returned")
	}
	finish()
}

func TestServiceRejectsMissingETagWhenRemoteIdentityChanged(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	object, err := service.Put(ctx, PutRequest{Key: "changed.txt", Body: strings.NewReader("data"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, "UPDATE r2_objects SET etag = '' WHERE object_id = ?", object.ObjectID); err != nil {
		t.Fatal(err)
	}
	backend.metadata["physical/"+object.PhysicalKey][InternalWriteIDMetadata] = "different-write"

	if _, err := service.Stat(ctx, object.Key); !errors.Is(err, ErrConditionalRequestConflict) {
		t.Fatalf("Stat error = %v", err)
	}
	indexed, err := service.Index.GetObject(ctx, object.Key)
	if err != nil {
		t.Fatal(err)
	}
	if indexed.ETag != "" {
		t.Fatalf("conflicting remote ETag was indexed: %#v", indexed)
	}
}

func TestServiceRejectsMissingETagWhenRemoteSizeChanged(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	object, err := service.Put(ctx, PutRequest{Key: "resized.txt", Body: strings.NewReader("data"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, "UPDATE r2_objects SET etag = '' WHERE object_id = ?", object.ObjectID); err != nil {
		t.Fatal(err)
	}
	physicalKey := "physical/" + object.PhysicalKey
	delete(backend.metadata[physicalKey], InternalWriteIDMetadata)
	backend.objects[physicalKey] = []byte("different-size")

	if _, err := service.Stat(ctx, object.Key); !errors.Is(err, ErrConditionalRequestConflict) {
		t.Fatalf("Stat error = %v", err)
	}
	indexed, err := service.Index.GetObject(ctx, object.Key)
	if err != nil {
		t.Fatal(err)
	}
	if indexed.ETag != "" {
		t.Fatalf("resized remote ETag was indexed: %#v", indexed)
	}
}

func TestServiceRepairsLegacyZeroSizeWithMatchingRemoteWriteID(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	object, err := service.Put(ctx, PutRequest{Key: "zero-size.txt", Body: strings.NewReader("data"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, `UPDATE r2_objects SET etag = '', size = 0 WHERE object_id = ?`, object.ObjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, `UPDATE r2_physical_buckets SET storage_bytes = 0 WHERE id = ?`, object.BucketID); err != nil {
		t.Fatal(err)
	}
	backend.etags["physical/"+object.PhysicalKey] = "repaired-etag"

	repaired, err := service.Stat(ctx, object.Key)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Size != 4 || repaired.ETag != "repaired-etag" {
		t.Fatalf("repaired object = %#v", repaired)
	}
	bucket, err := service.Index.GetBucket(ctx, object.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	if bucket.StorageBytes != 4 {
		t.Fatalf("bucket storage = %d, want 4", bucket.StorageBytes)
	}
}

func TestServiceZeroSizeRepairDoesNotDoubleCountScannedStorage(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	object, err := service.Put(ctx, PutRequest{Key: "scanned-zero-size.txt", Body: strings.NewReader("data"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, `UPDATE r2_objects SET etag = '', size = 0 WHERE object_id = ?`, object.ObjectID); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.FinishBucketScan(ctx, object.BucketID, 100, false); err != nil {
		t.Fatal(err)
	}
	backend.etags["physical/"+object.PhysicalKey] = "repaired-etag"

	if _, err := service.Stat(ctx, object.Key); err != nil {
		t.Fatal(err)
	}
	bucket, err := service.Index.GetBucket(ctx, object.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	if bucket.StorageBytes != 100 {
		t.Fatalf("scanned storage was double-counted: %d", bucket.StorageBytes)
	}
}

func TestServiceListsRepairLegacyObjectMetadata(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	object, err := service.Put(ctx, PutRequest{Key: "listed.txt", Body: strings.NewReader("data"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, `UPDATE r2_objects SET etag = '', size = 0 WHERE object_id = ?`, object.ObjectID); err != nil {
		t.Fatal(err)
	}
	backend.etags["physical/"+object.PhysicalKey] = "listed-etag"

	objects, err := service.List(ctx, ListOptions{Prefix: "listed", Limit: 10})
	if err != nil || len(objects.Objects) != 1 {
		t.Fatalf("List = %#v, error = %v", objects, err)
	}
	if objects.Objects[0].Size != 4 || objects.Objects[0].ETag != "listed-etag" {
		t.Fatalf("listed object = %#v", objects.Objects[0])
	}

	directoryObject, err := service.Put(ctx, PutRequest{Key: "directory-listed.txt", Body: strings.NewReader("file"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, `UPDATE r2_objects SET etag = '', size = 0 WHERE object_id = ?`, directoryObject.ObjectID); err != nil {
		t.Fatal(err)
	}
	backend.etags["physical/"+directoryObject.PhysicalKey] = "directory-etag"
	directory, err := service.ListDirectory(ctx, DirectoryListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, entry := range directory.Entries {
		if entry.Key == directoryObject.Key {
			found = true
			if entry.Size != 4 || entry.ETag != "directory-etag" {
				t.Fatalf("directory entry = %#v", entry)
			}
		}
	}
	if !found {
		t.Fatalf("directory listing = %#v", directory)
	}

	const credentialID = "webdav-list"
	webDAVKey := WebDAVMountPrefix(credentialID) + "save.dat"
	webDAVObject, err := service.Put(ctx, PutRequest{Key: webDAVKey, Body: strings.NewReader("save"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, `UPDATE r2_objects SET etag = '', size = 0 WHERE object_id = ?`, webDAVObject.ObjectID); err != nil {
		t.Fatal(err)
	}
	backend.etags["physical/"+webDAVObject.PhysicalKey] = "webdav-list-etag"
	webDAVList, err := service.ListWebDAVDirectory(ctx, credentialID, DirectoryListOptions{Limit: 100})
	if err != nil || len(webDAVList.Entries) != 1 {
		t.Fatalf("ListWebDAVDirectory = %#v, error = %v", webDAVList, err)
	}
	if webDAVList.Entries[0].Key != "save.dat" || webDAVList.Entries[0].Size != 4 ||
		webDAVList.Entries[0].ETag != "webdav-list-etag" {
		t.Fatalf("WebDAV directory entry = %#v", webDAVList.Entries[0])
	}
}

func TestServiceRepairsWeakLegacyETag(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	object, err := service.Put(ctx, PutRequest{Key: "weak-etag.txt", Body: strings.NewReader("data"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Index.db.ExecContext(ctx, `UPDATE r2_objects SET etag = ? WHERE object_id = ?`, `W/"legacy"`, object.ObjectID); err != nil {
		t.Fatal(err)
	}
	backend.etags["physical/"+object.PhysicalKey] = "strong-etag"

	repaired, err := service.Stat(ctx, object.Key)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.ETag != "strong-etag" || repaired.Size != object.Size {
		t.Fatalf("repaired object = %#v", repaired)
	}
}

type deleteFailureBackend struct{ *memoryBackend }

func (b *deleteFailureBackend) Delete(context.Context, Target, string) error {
	return errors.New("upstream delete failed")
}

func TestDeleteFailurePreservesCommittedMapping(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	object, err := service.Put(context.Background(), PutRequest{Key: "keep.txt", Body: strings.NewReader("keep"), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	service.Backend = &deleteFailureBackend{memoryBackend: backend}
	if err := service.Delete(context.Background(), object.Key); err == nil {
		t.Fatal("delete should fail")
	}
	current, err := service.Stat(context.Background(), object.Key)
	if err != nil || current.ObjectID != object.ObjectID {
		t.Fatalf("committed mapping = %#v, error = %v", current, err)
	}
	intents, err := service.Index.ListWriteIntents(context.Background(), 10)
	if err != nil || len(intents) != 0 {
		t.Fatalf("delete intent was not released after confirmed remote presence: %#v, %v", intents, err)
	}
}

func TestRecoveryCommitsRemoteObjectWithMatchingWriteID(t *testing.T) {
	t.Parallel()
	service, backend, _ := newChunkedTestService(t, 64)
	intent, err := service.Index.BeginWrite(context.Background(), BeginWriteInput{ObjectInput: ObjectInput{Key: "recover.bin", Size: 4}})
	if err != nil {
		t.Fatal(err)
	}
	target, err := service.target(context.Background(), intent.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), target, intent.Key, strings.NewReader("data"), 4, "", upstreamWriteMetadata(nil, intent.ID), PutOptions{IfNoneMatch: "*"}); err != nil {
		t.Fatal(err)
	}
	if err := service.Index.MarkWriteUploading(context.Background(), intent.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := service.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	object, err := service.Stat(context.Background(), intent.Key)
	if err != nil || object.ObjectID != intent.ID || object.Size != 4 {
		t.Fatalf("recovered object = %#v, error = %v", object, err)
	}
	bucket, _ := service.Index.GetBucket(context.Background(), intent.BucketID)
	if bucket.ReservedBytes != 0 || bucket.StorageBytes != 4 {
		t.Fatalf("bucket after recovery = %#v", bucket)
	}
}

type blockingPutBackend struct {
	*memoryBackend
	started chan struct{}
	release chan struct{}
}

func (b *blockingPutBackend) Put(
	ctx context.Context,
	target Target,
	key string,
	body io.Reader,
	size int64,
	contentType string,
	metadata map[string]string,
	options PutOptions,
) (string, error) {
	close(b.started)
	select {
	case <-b.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return b.memoryBackend.Put(ctx, target, key, body, size, contentType, metadata, options)
}

func TestRecoverySkipsActiveOrdinaryPut(t *testing.T) {
	t.Parallel()

	service, backend, _ := newChunkedTestService(t, 64)
	blocking := &blockingPutBackend{
		memoryBackend: backend,
		started:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	service.Backend = blocking
	ctx := context.Background()
	result := make(chan error, 1)
	go func() {
		_, err := service.Put(ctx, PutRequest{Key: "live.bin", Body: strings.NewReader("live"), Size: 4})
		result <- err
	}()
	<-blocking.started

	if err := service.RecoverInterrupted(ctx); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	intents, err := service.Index.ListWriteIntents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0].Key != "live.bin" {
		t.Fatalf("active write intent changed by recovery: %#v", intents)
	}

	close(blocking.release)
	if err := <-result; err != nil {
		t.Fatalf("Put: %v", err)
	}
	object, err := service.Stat(ctx, "live.bin")
	if err != nil || object.Size != 4 {
		t.Fatalf("committed object = %#v, error = %v", object, err)
	}
}

func TestLegacyRecoverySkipsActiveReplacementPut(t *testing.T) {
	t.Parallel()

	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	pending, err := service.Index.ReservePut(ctx, ObjectInput{Key: "legacy-live.bin", Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingPutBackend{
		memoryBackend: backend,
		started:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	service.Backend = blocking
	result := make(chan error, 1)
	go func() {
		_, err := service.Put(ctx, PutRequest{Key: pending.Key, Body: strings.NewReader("live"), Size: 4})
		result <- err
	}()
	<-blocking.started

	if err := service.RecoverInterrupted(ctx); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	legacy, err := service.Index.GetObjectByID(ctx, pending.ObjectID)
	if err != nil || legacy.State != StatePending {
		t.Fatalf("legacy row changed during active Put: %#v, %v", legacy, err)
	}

	close(blocking.release)
	if err := <-result; err != nil {
		t.Fatalf("Put: %v", err)
	}
	current, err := service.Stat(ctx, pending.Key)
	if err != nil || current.ObjectID == pending.ObjectID || current.State != StateCommitted {
		t.Fatalf("replacement object = %#v, error = %v", current, err)
	}
	intents, err := service.Index.ListWriteIntents(ctx, 10)
	if err != nil || len(intents) != 0 {
		t.Fatalf("write intents after replacement = %#v, error = %v", intents, err)
	}
}

type memoryBackend struct {
	objects         map[string][]byte
	metadata        map[string]map[string]string
	etags           map[string]string
	uploads         map[string]map[int32][]byte
	targets         map[string]string
	uploadMetadata  map[string]map[string]string
	putCalls        int
	putError        error
	createError     error
	completeError   error
	abortError      error
	putOptions      PutOptions
	completeOptions PutOptions
	getOptions      GetOptions
	getCalls        int
	headCalls       int
}

func (b *memoryBackend) Put(_ context.Context, target Target, key string, body io.Reader, _ int64, _ string, metadata map[string]string, options PutOptions) (string, error) {
	b.putCalls++
	b.putOptions = options
	if b.putError != nil {
		return "", b.putError
	}
	data, _ := io.ReadAll(body)
	physicalKey := target.Bucket + "/" + key
	b.objects[physicalKey] = data
	if b.metadata == nil {
		b.metadata = make(map[string]map[string]string)
	}
	b.metadata[physicalKey] = cloneMetadata(metadata)
	if b.etags == nil {
		b.etags = make(map[string]string)
	}
	b.etags[physicalKey] = "etag"
	return "etag", nil
}

func (b *memoryBackend) Get(_ context.Context, target Target, key string, options GetOptions) (GetResult, error) {
	b.getCalls++
	b.getOptions = options
	data, ok := b.objects[target.Bucket+"/"+key]
	if !ok {
		return GetResult{}, ErrObjectNotFound
	}
	return GetResult{Body: io.NopCloser(bytes.NewReader(data)), Size: int64(len(data)), ETag: b.etags[target.Bucket+"/"+key],
		Metadata: userVisibleMetadata(b.metadata[target.Bucket+"/"+key])}, nil
}

type blockingGetBackend struct {
	*memoryBackend
	started chan struct{}
	release chan struct{}
}

func (b *blockingGetBackend) Get(ctx context.Context, target Target, key string, options GetOptions) (GetResult, error) {
	close(b.started)
	select {
	case <-b.release:
	case <-ctx.Done():
		return GetResult{}, ctx.Err()
	}
	return b.memoryBackend.Get(ctx, target, key, options)
}

func (b *memoryBackend) Delete(_ context.Context, target Target, key string) error {
	delete(b.objects, target.Bucket+"/"+key)
	delete(b.metadata, target.Bucket+"/"+key)
	delete(b.etags, target.Bucket+"/"+key)
	return nil
}

func (b *memoryBackend) CreateMultipart(_ context.Context, target Target, key, _ string, metadata map[string]string) (string, error) {
	if b.createError != nil {
		return "", b.createError
	}
	if b.uploads == nil {
		b.uploads = make(map[string]map[int32][]byte)
		b.targets = make(map[string]string)
		b.uploadMetadata = make(map[string]map[string]string)
	}
	id := key + "-upload"
	b.uploads[id] = make(map[int32][]byte)
	b.targets[id] = target.Bucket + "/" + key
	b.uploadMetadata[id] = cloneMetadata(metadata)
	return id, nil
}

func (b *memoryBackend) UploadPart(_ context.Context, _ Target, _ string, uploadID string, partNumber int32, body io.Reader, _ int64) (string, error) {
	data, _ := io.ReadAll(body)
	b.uploads[uploadID][partNumber] = data
	return "etag-" + string(rune('0'+partNumber)), nil
}

func (b *memoryBackend) CompleteMultipart(_ context.Context, _ Target, _ string, uploadID string, parts []CompletedPart, options PutOptions) (string, error) {
	b.completeOptions = options
	if b.completeError != nil {
		if errors.Is(b.completeError, ErrConditionalRequestConflict) || isMultipartNotFound(b.completeError) {
			delete(b.uploads, uploadID)
			delete(b.targets, uploadID)
			delete(b.uploadMetadata, uploadID)
		}
		return "", b.completeError
	}
	var data []byte
	for _, part := range parts {
		data = append(data, b.uploads[uploadID][part.PartNumber]...)
	}
	target := b.targets[uploadID]
	b.objects[target] = data
	if b.metadata == nil {
		b.metadata = make(map[string]map[string]string)
	}
	b.metadata[target] = cloneMetadata(b.uploadMetadata[uploadID])
	if b.etags == nil {
		b.etags = make(map[string]string)
	}
	b.etags[target] = "multipart-etag"
	delete(b.uploads, uploadID)
	delete(b.targets, uploadID)
	delete(b.uploadMetadata, uploadID)
	return "multipart-etag", nil
}

func (b *memoryBackend) AbortMultipart(_ context.Context, _ Target, _ string, uploadID string) error {
	if b.abortError != nil {
		return b.abortError
	}
	delete(b.uploads, uploadID)
	delete(b.targets, uploadID)
	delete(b.uploadMetadata, uploadID)
	return nil
}

type testRemoteError struct {
	code   string
	status int
}

func (e testRemoteError) Error() string       { return e.code }
func (e testRemoteError) ErrorCode() string   { return e.code }
func (e testRemoteError) HTTPStatusCode() int { return e.status }

func TestIsRemoteNotFound(t *testing.T) {
	if !isRemoteNotFound(testRemoteError{code: "NoSuchKey"}) {
		t.Fatal("NoSuchKey should be treated as not found")
	}
	if !isRemoteNotFound(testRemoteError{code: "AccessDenied", status: 404}) {
		t.Fatal("HTTP 404 should be treated as not found")
	}
	if isRemoteNotFound(testRemoteError{code: "AccessDenied", status: 403}) {
		t.Fatal("HTTP 403 must not be treated as not found")
	}
}

func (b *memoryBackend) Head(_ context.Context, target Target, key string) (RemoteObject, error) {
	b.headCalls++
	data, ok := b.objects[target.Bucket+"/"+key]
	if !ok {
		return RemoteObject{}, ErrObjectNotFound
	}
	etag := b.etags[target.Bucket+"/"+key]
	if etag == "" {
		etag = "remote-etag"
	}
	return RemoteObject{Key: key, Size: int64(len(data)), ETag: etag,
		Metadata: cloneMetadata(b.metadata[target.Bucket+"/"+key]), LastModified: time.Now()}, nil
}

func (b *memoryBackend) ListRemote(_ context.Context, target Target, prefix, continuation string, limit int32) (RemoteObjectList, error) {
	var keys []string
	bucketPrefix := target.Bucket + "/"
	for physicalKey := range b.objects {
		if !strings.HasPrefix(physicalKey, bucketPrefix) {
			continue
		}
		key := strings.TrimPrefix(physicalKey, bucketPrefix)
		if strings.HasPrefix(key, prefix) && key > continuation {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	result := RemoteObjectList{}
	for index, key := range keys {
		if int32(index) == limit {
			result.ContinuationToken = keys[index-1]
			break
		}
		result.Objects = append(result.Objects, RemoteObject{
			Key: key, Size: int64(len(b.objects[bucketPrefix+key])), ETag: "remote-etag", LastModified: time.Now(),
		})
	}
	return result, nil
}
