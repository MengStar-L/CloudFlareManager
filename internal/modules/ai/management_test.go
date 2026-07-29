package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestManagementListsWorkersAIModels(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/cloudflare/ai/models/search" || r.URL.Query().Get("per_page") != "100" {
			t.Fatalf("URL = %s", r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result":  []any{map[string]any{"name": "@cf/meta/llama", "task": map[string]any{"name": "Text Generation"}}},
		})
	}))
	defer server.Close()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{12}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	management := Management{Accounts: accountStore, BaseURL: server.URL, HTTPClient: server.Client()}
	models, err := management.ListModels(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0]["name"] != "@cf/meta/llama" {
		t.Fatalf("models = %#v", models)
	}
}

func TestManagementFiltersPaidModels(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": []any{
				map[string]any{"name": "@cf/zai-org/glm-5.2"},
				map[string]any{"name": "@cf/meta/llama-3.2-1b-instruct"},
				map[string]any{"name": "@cf/vendor/learned-paid"},
				map[string]any{"name": "@cf/meta/llama-3.2-1b-instruct"},
			},
		})
	}))
	defer server.Close()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{17}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := NewModelPolicy(db)
	if err := policy.LearnPaid(context.Background(), "@cf/vendor/learned-paid", "requires a Workers Paid plan"); err != nil {
		t.Fatal(err)
	}
	models, err := (Management{
		Accounts: accountStore, Policy: policy, BaseURL: server.URL, HTTPClient: server.Client(),
	}).ListModels(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := modelIDs(models); !reflect.DeepEqual(got, []string{"@cf/meta/llama-3.2-1b-instruct"}) {
		t.Fatalf("models = %#v", got)
	}
}

func TestManagementPaginatesWorkersAIModels(t *testing.T) {
	t.Parallel()

	pages := make([]int, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if value := r.URL.Query().Get("page"); value == "2" {
			page = 2
		}
		pages = append(pages, page)
		models := make([]map[string]any, 0, 50)
		if page == 1 {
			for index := 0; index < 50; index++ {
				models = append(models, map[string]any{"name": fmt.Sprintf("@cf/model/%03d", index)})
			}
		} else {
			models = append(models, map[string]any{"name": "@cf/model/050"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":     true,
			"result":      models,
			"result_info": map[string]any{"page": page, "per_page": 50},
		})
	}))
	defer server.Close()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{15}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}

	models, err := (Management{Accounts: accountStore, BaseURL: server.URL, HTTPClient: server.Client()}).ListModels(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 51 {
		t.Fatalf("model count = %d, want 51", len(models))
	}
	if !reflect.DeepEqual(pages, []int{1, 2}) {
		t.Fatalf("pages = %#v", pages)
	}
}

func TestManagementRejectsRepeatedWorkersAIModelPage(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		models := make([]map[string]any, 0, 100)
		for index := 0; index < 100; index++ {
			models = append(models, map[string]any{"name": fmt.Sprintf("@cf/model/%03d", index)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result":  models,
			"result_info": map[string]any{
				"per_page": 100, "total_pages": 50,
			},
		})
	}))
	defer server.Close()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{16}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = (Management{Accounts: accountStore, BaseURL: server.URL, HTTPClient: server.Client()}).ListModels(context.Background(), account.ID)
	if err == nil {
		t.Fatal("ListModels should reject a repeated upstream page")
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestCreateGatewayFillsRequiredDefaults(t *testing.T) {
	t.Parallel()

	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/accounts/cloudflare/ai-gateway/gateways" {
			t.Fatalf("URL = %s %s", r.Method, r.URL.String())
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"id": "test"}})
	}))
	defer server.Close()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{13}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	management := Management{Accounts: accountStore, BaseURL: server.URL, HTTPClient: server.Client()}
	if _, err := management.CreateGateway(context.Background(), account.ID, map[string]any{"id": "test"}); err != nil {
		t.Fatal(err)
	}
	// Cloudflare 校验这些字段必填；缺省时报 "Expected number, received nan"。
	for key, want := range map[string]any{
		"id": "test", "cache_ttl": float64(0), "collect_logs": true,
		"rate_limiting_interval": float64(0), "rate_limiting_limit": float64(0),
		"rate_limiting_technique": "fixed", "cache_invalidate_on_update": false,
	} {
		if received[key] != want {
			t.Fatalf("payload[%q] = %#v, want %#v", key, received[key], want)
		}
	}
}

func TestPickAccountPrefersAICapableAccounts(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{14}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	management := Management{Accounts: accountStore}

	if _, err := management.PickAccount(context.Background()); err == nil {
		t.Fatal("PickAccount with no accounts should fail")
	}

	noAI, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "r2-only", CloudflareAccountID: "cf-1", APIToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	withAI, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "ai-ready", CloudflareAccountID: "cf-2", APIToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	for id, capabilities := range map[string][]accounts.Capability{
		noAI.ID:   {{Name: "r2", Available: true}},
		withAI.ID: {{Name: "ai", Available: true}},
	} {
		if err := accountStore.SetCapabilities(context.Background(), id, capabilities); err != nil {
			t.Fatal(err)
		}
		if err := accountStore.SetHealth(context.Background(), id, "healthy", ""); err != nil {
			t.Fatal(err)
		}
	}
	picked, err := management.PickAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != withAI.ID {
		t.Fatalf("picked %q, want AI-capable %q", picked.Name, withAI.Name)
	}
}
