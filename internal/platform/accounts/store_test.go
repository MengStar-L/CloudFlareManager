package accounts

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestStoreEncryptsCredentialsAndRoundTripsAccount(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{7}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, secret.NewRepository(db, cipher))

	created, err := store.Create(context.Background(), CreateInput{
		Name: "primary", CloudflareAccountID: "cf-account", APIToken: "api-secret",
		R2AccessKeyID: "r2-key", R2SecretAccessKey: "r2-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), created.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIToken != "api-secret" || got.R2SecretAccessKey != "r2-secret" {
		t.Fatalf("credentials did not round trip: %#v", got)
	}

	var plaintextCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM encrypted_secrets WHERE CAST(ciphertext AS TEXT) LIKE '%api-secret%'").Scan(&plaintextCount); err != nil {
		t.Fatal(err)
	}
	if plaintextCount != 0 {
		t.Fatal("plaintext token found in database")
	}
}
