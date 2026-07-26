package r2

import (
	"bytes"
	"context"
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
	if _, err := index.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "physical"}); err != nil {
		t.Fatal(err)
	}
	backend := &memoryBackend{objects: map[string][]byte{}}
	service := Service{Index: index, Accounts: accountStore, Backend: backend, TempDir: t.TempDir()}

	object, err := service.Put(context.Background(), PutRequest{
		Key: "docs/readme.txt", Body: strings.NewReader("hello"), Size: 5,
		ContentType: "text/plain", PayloadHash: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
	})
	if err != nil {
		t.Fatal(err)
	}
	if object.State != StateCommitted {
		t.Fatalf("object state = %q", object.State)
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
	if err := service.Delete(context.Background(), "docs/readme.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := index.GetObject(context.Background(), "docs/readme.txt"); err != ErrObjectNotFound {
		t.Fatalf("deleted object error = %v", err)
	}
}

type memoryBackend struct {
	objects map[string][]byte
	uploads map[string]map[int32][]byte
	targets map[string]string
}

func (b *memoryBackend) Put(_ context.Context, target Target, key string, body io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
	data, _ := io.ReadAll(body)
	b.objects[target.Bucket+"/"+key] = data
	return "etag", nil
}

func (b *memoryBackend) Get(_ context.Context, target Target, key string, _ GetOptions) (GetResult, error) {
	data, ok := b.objects[target.Bucket+"/"+key]
	if !ok {
		return GetResult{}, ErrObjectNotFound
	}
	return GetResult{Body: io.NopCloser(bytes.NewReader(data)), Size: int64(len(data)), ETag: "etag"}, nil
}

func (b *memoryBackend) Delete(_ context.Context, target Target, key string) error {
	delete(b.objects, target.Bucket+"/"+key)
	return nil
}

func (b *memoryBackend) CreateMultipart(_ context.Context, target Target, key, _ string, _ map[string]string) (string, error) {
	if b.uploads == nil {
		b.uploads = make(map[string]map[int32][]byte)
		b.targets = make(map[string]string)
	}
	id := key + "-upload"
	b.uploads[id] = make(map[int32][]byte)
	b.targets[id] = target.Bucket + "/" + key
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
	b.objects[b.targets[uploadID]] = data
	delete(b.uploads, uploadID)
	return "multipart-etag", nil
}

func (b *memoryBackend) AbortMultipart(_ context.Context, _ Target, _ string, uploadID string) error {
	delete(b.uploads, uploadID)
	delete(b.targets, uploadID)
	return nil
}

func (b *memoryBackend) Head(_ context.Context, target Target, key string) (RemoteObject, error) {
	data, ok := b.objects[target.Bucket+"/"+key]
	if !ok {
		return RemoteObject{}, ErrObjectNotFound
	}
	return RemoteObject{Key: key, Size: int64(len(data)), ETag: "remote-etag", LastModified: time.Now()}, nil
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
