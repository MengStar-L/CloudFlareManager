package d1

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestClientQueriesD1AndRecordsHistory(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/accounts/cloudflare/d1/database/database/query" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": []any{map[string]any{
				"success": true, "results": []any{map[string]any{"answer": 42}},
				"meta": map[string]any{"duration": 1.5, "rows_read": 1, "rows_written": 0},
			}},
		})
	}))
	defer server.Close()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{6}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	client := Client{BaseURL: server.URL, HTTPClient: server.Client(), Accounts: accountStore, DB: db}
	results, err := client.Query(context.Background(), account.ID, "database", QueryInput{SQL: "SELECT 42 AS answer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Meta.RowsRead != 1 {
		t.Fatalf("results = %#v", results)
	}
	history, err := client.History(context.Background(), account.ID, "database", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Class != SQLRead {
		t.Fatalf("history = %#v", history)
	}
}
