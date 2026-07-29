package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenAppliesMigrationsAndPragmas(t *testing.T) {
	t.Parallel()

	db, err := Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var journal string
	if err := db.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Fatalf("journal mode = %q", journal)
	}

	var count int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("expected applied migration")
	}

	var paidModelsTable int
	if err := db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'ai_paid_models'`).Scan(&paidModelsTable); err != nil {
		t.Fatal(err)
	}
	if paidModelsTable != 1 {
		t.Fatal("ai_paid_models migration was not applied")
	}
	rows, err := db.QueryContext(context.Background(), "PRAGMA table_info(ai_request_logs)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	foundSource := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		foundSource = foundSource || name == "neuron_estimation_source"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !foundSource {
		t.Fatal("neuron_estimation_source column was not applied")
	}
}
