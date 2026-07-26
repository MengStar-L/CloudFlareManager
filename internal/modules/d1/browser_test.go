package d1

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestTableRowsQuotesIdentifierAndPaginates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input QueryInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		var rows []map[string]any
		switch {
		case strings.Contains(input.SQL, "sqlite_schema"):
			rows = []map[string]any{{"name": `odd"table`}}
		case strings.HasPrefix(input.SQL, "PRAGMA table_xinfo"):
			if input.SQL != `PRAGMA table_xinfo("odd""table")` {
				t.Fatalf("pragma SQL = %q", input.SQL)
			}
			rows = []map[string]any{{"cid": 0, "name": "id"}, {"cid": 1, "name": "value"}}
		case strings.HasPrefix(input.SQL, "SELECT *"):
			if input.SQL != `SELECT * FROM "odd""table" LIMIT ? OFFSET ?` {
				t.Fatalf("select SQL = %q", input.SQL)
			}
			rows = []map[string]any{{"id": 1}, {"id": 2}, {"id": 3}}
		default:
			t.Fatalf("unexpected SQL = %q", input.SQL)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": []any{map[string]any{
				"success": true, "results": rows,
				"meta": map[string]any{"duration": 1, "rows_read": len(rows), "rows_written": 0},
			}},
		})
	}))
	defer server.Close()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{11}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	client := Client{BaseURL: server.URL, HTTPClient: server.Client(), Accounts: accountStore, DB: db}
	page, err := client.TableRows(context.Background(), account.ID, "database", `odd"table`, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Columns) != 2 || len(page.Rows) != 2 || !page.HasMore {
		t.Fatalf("page = %#v", page)
	}
}

func TestAnalyzeHistoryEntryFlagsCostlyQueries(t *testing.T) {
	t.Parallel()

	category, severity, _ := analyzeHistoryEntry(HistoryEntry{Success: true, RowsRead: 120000, DurationMS: 100})
	if category != "rows_read" || severity != "high" {
		t.Fatalf("insight = %q %q", category, severity)
	}
	category, _, _ = analyzeHistoryEntry(HistoryEntry{Success: true, RowsRead: 10, DurationMS: 10})
	if category != "" {
		t.Fatalf("unexpected category = %q", category)
	}
}
