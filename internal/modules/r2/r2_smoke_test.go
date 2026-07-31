package r2

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
	"github.com/google/uuid"
)

func TestRealR2TwoAccountSmoke(t *testing.T) {
	if os.Getenv("CF_R2_SMOKE") != "1" {
		t.Skip("set CF_R2_SMOKE=1 and the CF_R2_SMOKE_ACCOUNT_* variables to run")
	}
	type credentials struct {
		accountID, accessKey, secretKey, bucket string
	}
	load := func(suffix string) credentials {
		return credentials{
			accountID: os.Getenv("CF_R2_SMOKE_ACCOUNT_" + suffix + "_ID"),
			accessKey: os.Getenv("CF_R2_SMOKE_ACCOUNT_" + suffix + "_ACCESS_KEY_ID"),
			secretKey: os.Getenv("CF_R2_SMOKE_ACCOUNT_" + suffix + "_SECRET_ACCESS_KEY"),
			bucket:    os.Getenv("CF_R2_SMOKE_ACCOUNT_" + suffix + "_BUCKET"),
		}
	}
	firstCredentials, secondCredentials := load("1"), load("2")
	for index, item := range []credentials{firstCredentials, secondCredentials} {
		if item.accountID == "" || item.accessKey == "" || item.secretKey == "" || item.bucket == "" {
			t.Fatalf("real R2 smoke account %d credentials are incomplete", index+1)
		}
	}

	ctx := context.Background()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{31}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	index := NewStore(db, Limits{StorageBytes: 16, AccountStorageBytes: 16, ClassA: 100, ClassB: 100})
	var buckets []PhysicalBucket
	for number, item := range []credentials{firstCredentials, secondCredentials} {
		account, err := accountStore.Create(ctx, accounts.CreateInput{
			Name: "smoke-" + string(rune('1'+number)), CloudflareAccountID: item.accountID, APIToken: "smoke-not-used",
			R2AccessKeyID: item.accessKey, R2SecretAccessKey: item.secretKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		bucket, err := index.CreateBucket(ctx, CreateBucketInput{AccountID: account.ID, Name: item.bucket})
		if err != nil {
			t.Fatal(err)
		}
		if err := index.FinishBucketScan(ctx, bucket.ID, 0, false); err != nil {
			t.Fatal(err)
		}
		buckets = append(buckets, bucket)
	}
	if _, err := db.ExecContext(ctx, `UPDATE r2_physical_buckets SET storage_bytes = 15 WHERE id = ?`, buckets[0].ID); err != nil {
		t.Fatal(err)
	}

	backend := AWSBackend{}
	service := Service{Index: index, Accounts: accountStore, Backend: backend, TempDir: t.TempDir(), ChunkBytes: 5 << 20}
	prefix := "cf-r2-manager-smoke/" + uuid.NewString() + "/"
	objectKey, multipartKey := prefix+"object.txt", prefix+"multipart.bin"
	var activeUploadID string
	cleanup := func(verify bool) {
		for _, bucket := range buckets {
			target, targetErr := service.target(context.Background(), bucket.ID)
			if targetErr == nil {
				if activeUploadID != "" {
					_ = backend.AbortMultipart(context.Background(), target, multipartKey, activeUploadID)
				}
				_ = backend.Delete(context.Background(), target, objectKey)
				_ = backend.Delete(context.Background(), target, multipartKey)
				if verify {
					for _, key := range []string{objectKey, multipartKey} {
						if _, err := backend.Head(context.Background(), target, key); !isRemoteNotFound(err) {
							t.Errorf("smoke object remains in %s: key=%s error=%v", bucket.Name, key, err)
						}
					}
				}
			}
		}
	}
	defer cleanup(false)

	object, err := service.Put(ctx, PutRequest{Key: objectKey, Body: strings.NewReader("ok"), Size: 2})
	if err != nil {
		t.Fatal(err)
	}
	if object.BucketID != buckets[1].ID {
		t.Fatalf("object bucket = %s, want second account bucket %s", object.BucketID, buckets[1].ID)
	}
	result, err := service.Get(ctx, objectKey, GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = result.Body.Close()
	if err := service.Delete(ctx, objectKey); err != nil {
		t.Fatal(err)
	}

	upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: multipartKey})
	if err != nil {
		t.Fatal(err)
	}
	activeUploadID = upload.UpstreamID
	if _, err := service.UploadPart(ctx, UploadPartRequest{
		Key: multipartKey, UploadID: upload.ID, PartNumber: 1, Body: strings.NewReader("part"), Size: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.AbortMultipart(ctx, multipartKey, upload.ID); err != nil {
		t.Fatal(err)
	}
	activeUploadID = ""
	if _, err := index.GetMultipart(ctx, upload.ID); !errors.Is(err, ErrMultipartNotFound) {
		t.Fatalf("multipart row remains after abort: %v", err)
	}
	cleanup(true)
}
