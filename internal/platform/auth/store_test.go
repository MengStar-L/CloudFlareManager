package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
)

func TestStoreAdminAndSessionLifecycle(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)

	if err := store.InitializeAdmin(context.Background(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if err := store.Authenticate(context.Background(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if err := store.Authenticate(context.Background(), "wrong password"); err == nil {
		t.Fatal("expected authentication failure")
	}

	session, err := store.CreateSession(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateSession(context.Background(), session.Token); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeSession(context.Background(), session.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateSession(context.Background(), session.Token); err == nil {
		t.Fatal("expected revoked session to fail")
	}
}
