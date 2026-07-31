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
	if _, err := backend.Put(context.Background(), target, intent.Key, strings.NewReader("data"), 4, "", upstreamWriteMetadata(nil, intent.ID)); err != nil {
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

type memoryBackend struct {
	objects        map[string][]byte
	metadata       map[string]map[string]string
	etags          map[string]string
	uploads        map[string]map[int32][]byte
	targets        map[string]string
	uploadMetadata map[string]map[string]string
}

func (b *memoryBackend) Put(_ context.Context, target Target, key string, body io.Reader, _ int64, _ string, metadata map[string]string) (string, error) {
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

func (b *memoryBackend) Get(_ context.Context, target Target, key string, _ GetOptions) (GetResult, error) {
	data, ok := b.objects[target.Bucket+"/"+key]
	if !ok {
		return GetResult{}, ErrObjectNotFound
	}
	return GetResult{Body: io.NopCloser(bytes.NewReader(data)), Size: int64(len(data)), ETag: b.etags[target.Bucket+"/"+key],
		Metadata: userVisibleMetadata(b.metadata[target.Bucket+"/"+key])}, nil
}

func (b *memoryBackend) Delete(_ context.Context, target Target, key string) error {
	delete(b.objects, target.Bucket+"/"+key)
	delete(b.metadata, target.Bucket+"/"+key)
	delete(b.etags, target.Bucket+"/"+key)
	return nil
}

func (b *memoryBackend) CreateMultipart(_ context.Context, target Target, key, _ string, metadata map[string]string) (string, error) {
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

func (b *memoryBackend) CompleteMultipart(_ context.Context, _ Target, _ string, uploadID string, parts []CompletedPart) (string, error) {
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
