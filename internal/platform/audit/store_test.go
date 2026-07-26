package audit

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
)

func TestStoreRecordsAndListsNewestFirst(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewStore(db)
	ctx := context.Background()

	for _, action := range []string{"account.create", "account.delete"} {
		if _, err := store.Record(ctx, Event{Actor: "admin", Protocol: "admin", Action: action, Resource: "accounts/a1", Result: "success", RequestID: action}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.List(ctx, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Action != "account.delete" {
		t.Fatalf("events = %#v", events)
	}
}
