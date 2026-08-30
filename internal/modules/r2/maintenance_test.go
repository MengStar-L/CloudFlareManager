package r2

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestMaintenanceAdoptScanRecoverAndRebalance(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{10}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
		R2AccessKeyID: "access", R2SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	index := NewStore(db, Limits{StorageBytes: 1000, ClassA: 100, ClassB: 100})
	source, err := index.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "source"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := index.CreateBucket(context.Background(), CreateBucketInput{AccountID: account.ID, Name: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if err := index.FinishBucketScan(context.Background(), target.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	backend := &memoryBackend{objects: map[string][]byte{"source/legacy.txt": []byte("legacy")}}
	service := Service{Index: index, Accounts: accountStore, Backend: backend, TempDir: t.TempDir()}

	report, err := service.AdoptBucket(context.Background(), source.ID)
	if err != nil || report.Imported != 1 {
		t.Fatalf("adopt report = %#v, error = %v", report, err)
	}
	backend.objects["source/orphan.txt"] = []byte("orphan")
	report, err = service.ScanOrphans(context.Background(), source.ID)
	if err != nil || report.Orphans != 1 {
		t.Fatalf("orphan report = %#v, error = %v", report, err)
	}
	findings, err := index.ListScanFindings(context.Background(), source.ID, OrphanFinding, 10)
	if err != nil || len(findings) != 1 || findings[0].Key != "orphan.txt" {
		t.Fatalf("findings = %#v, error = %v", findings, err)
	}

	pending, err := index.ReservePut(context.Background(), ObjectInput{Key: "recover.txt", Size: 7})
	if err != nil {
		t.Fatal(err)
	}
	backend.objects["source/recover.txt"] = []byte("recover")
	if pending.BucketID != source.ID {
		backend.objects["target/recover.txt"] = backend.objects["source/recover.txt"]
	}
	if err := service.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recovered, err := index.GetObject(context.Background(), "recover.txt"); err != nil || recovered.State != StateCommitted {
		t.Fatalf("recovered object = %#v, error = %v", recovered, err)
	}

	moved, err := service.Rebalance(context.Background(), source.ID, target.ID, "legacy")
	if err != nil || moved != 1 {
		t.Fatalf("moved = %d, error = %v", moved, err)
	}
	object, err := index.GetObject(context.Background(), "legacy.txt")
	if err != nil || object.BucketID != target.ID || string(backend.objects["target/legacy.txt"]) != "legacy" {
		t.Fatalf("rebalanced object = %#v, error = %v", object, err)
	}
}

func TestMoveObjectSkipsStaleRebalanceSnapshot(t *testing.T) {
	t.Parallel()

	service, backend, _ := newChunkedTestService(t, 64)
	ctx := context.Background()
	first, err := service.Put(ctx, PutRequest{Key: "changing.txt", Body: strings.NewReader("old"), Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Index.GetObject(ctx, first.Key)
	if err != nil {
		t.Fatal(err)
	}
	current, err := service.Put(ctx, PutRequest{Key: first.Key, Body: strings.NewReader("new-version"), Size: 11})
	if err != nil {
		t.Fatal(err)
	}
	source, err := service.Index.GetBucket(ctx, snapshot.BucketID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := service.Index.CreateBucket(ctx, CreateBucketInput{AccountID: source.AccountID, Name: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Index.FinishBucketScan(ctx, target.ID, 0, false); err != nil {
		t.Fatal(err)
	}

	if err := service.moveObject(ctx, snapshot, target.ID); !errors.Is(err, errRebalanceObjectChanged) {
		t.Fatalf("moveObject error = %v, want stale-snapshot sentinel", err)
	}
	indexed, err := service.Index.GetObject(ctx, first.Key)
	if err != nil {
		t.Fatal(err)
	}
	if indexed.ObjectID != current.ObjectID || indexed.BucketID != source.ID {
		t.Fatalf("current object changed by stale move: %#v", indexed)
	}
	if got := string(backend.objects[source.Name+"/"+first.Key]); got != "new-version" {
		t.Fatalf("source bytes = %q, want new-version", got)
	}
	if _, found := backend.objects[target.Name+"/"+first.Key]; found {
		t.Fatal("stale snapshot was copied into target bucket")
	}
}

func TestRecoverUnboundLegacyMultipartPreservesNoSuchUploadAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name        string
		remote      bool
		headFailure bool
	}{
		{name: "remote object exists", remote: true},
		{name: "HEAD is uncertain", headFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, backend, _ := newChunkedTestService(t, 64)
			ctx := context.Background()
			upload, err := service.CreateMultipart(ctx, CreateMultipartInput{Key: "legacy-unbound.bin"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Index.db.ExecContext(ctx,
				"UPDATE r2_multipart_uploads SET write_intent_id = NULL WHERE id = ?", upload.ID); err != nil {
				t.Fatal(err)
			}
			if err := service.Index.AbortWrite(ctx, upload.WriteIntentID); err != nil {
				t.Fatal(err)
			}
			backend.abortError = testRemoteError{code: "NoSuchUpload", status: 404}
			target, err := service.target(ctx, upload.BucketID)
			if err != nil {
				t.Fatal(err)
			}
			if test.remote {
				physicalKey := target.Bucket + "/" + upload.Key
				backend.objects[physicalKey] = []byte("published")
				if backend.etags == nil {
					backend.etags = make(map[string]string)
				}
				backend.etags[physicalKey] = "published-etag"
			}
			if test.headFailure {
				service.Backend = &headFailureForKeyBackend{memoryBackend: backend, key: upload.Key}
			}

			err = service.recoverUnboundLegacyMultipart(ctx)
			if !errors.Is(err, ErrWriteRecoveryAmbiguous) {
				t.Fatalf("recovery error = %v", err)
			}
			if _, err := service.Index.GetMultipart(ctx, upload.ID); err != nil {
				t.Fatalf("multipart was removed after ambiguous NoSuchUpload: %v", err)
			}
		})
	}
}
