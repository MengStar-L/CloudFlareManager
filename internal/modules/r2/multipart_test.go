package r2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

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
	if _, err := index.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "physical"}); err != nil {
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
