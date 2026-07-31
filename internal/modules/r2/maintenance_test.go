package r2

import (
	"bytes"
	"context"
	"path/filepath"
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
