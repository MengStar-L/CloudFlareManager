package credentials

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestStoreCredentialLifecycle(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{8}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, secret.NewRepository(db, cipher))
	created, err := store.Create(context.Background(), CreateInput{Kind: KindS3, Name: "backup client", Scopes: []string{"r2:read", "r2:write"}})
	if err != nil {
		t.Fatal(err)
	}
	if created.Secret == "" || created.PublicID == "" {
		t.Fatalf("created credential = %#v", created)
	}
	if _, err := store.Verify(context.Background(), KindS3, created.PublicID, created.Secret); err != nil {
		t.Fatal(err)
	}
	secretValue, credential, err := store.Secret(context.Background(), KindS3, created.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if secretValue != created.Secret || credential.ID != created.ID {
		t.Fatalf("secret lookup mismatch")
	}
	if err := store.Revoke(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Verify(context.Background(), KindS3, created.PublicID, created.Secret); err != ErrInvalidCredential {
		t.Fatalf("verify revoked credential error = %v", err)
	}
}

func TestDeleteRequiresRevokedCredential(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{9}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, secret.NewRepository(db, cipher))
	created, err := store.Create(context.Background(), CreateInput{Kind: KindS3, Name: "cleanup target", Scopes: []string{"r2:read"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), created.ID); err != ErrNotRevoked {
		t.Fatalf("delete active credential error = %v, want ErrNotRevoked", err)
	}
	if err := store.Revoke(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), created.ID); err != ErrNotFound {
		t.Fatalf("delete missing credential error = %v, want ErrNotFound", err)
	}
	items, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == created.ID {
			t.Fatalf("deleted credential still listed")
		}
	}
}
