package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestGatewayPassesThroughSSEAndRecordsUsage(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/cloudflare/ai/v1/chat/completions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{7}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := accountStore.SetCapabilities(context.Background(), account.ID, []accounts.Capability{{Name: "ai", Available: true, CheckedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err := accountStore.SetHealth(context.Background(), account.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}

	gateway := Gateway{Accounts: accountStore, DB: db, BaseURL: upstream.URL, HTTPClient: upstream.Client(), NeuronSoftLimit: 9000}
	body, _ := json.Marshal(map[string]any{"model": "@cf/meta/llama", "stream": true, "messages": []any{map[string]string{"role": "user", "content": "hello"}}})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	if err := gateway.Forward(response, request, "credential"); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "[DONE]") {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	var requests int
	if err := db.QueryRow("SELECT requests FROM ai_usage_daily WHERE account_id = ?", account.ID).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestGatewayTranslatesResponsesToChatAndBack(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/cloudflare/ai/v1/chat/completions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["messages"]; !ok {
			t.Fatalf("translated body has no messages: %#v", body)
		}
		if _, ok := body["input"]; ok {
			t.Fatalf("translated body retained input: %#v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, _ := secret.NewCipher(bytes.Repeat([]byte{8}, secret.KeySize))
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := accountStore.SetCapabilities(context.Background(), account.ID, []accounts.Capability{{Name: "ai", Available: true, CheckedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err := accountStore.SetHealth(context.Background(), account.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}

	gateway := Gateway{Accounts: accountStore, DB: db, BaseURL: upstream.URL, HTTPClient: upstream.Client(), NeuronSoftLimit: 9000}
	body := []byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"instructions":"help","stream":true}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	if err := gateway.Forward(response, request, "credential"); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "response.output_text.delta") || !strings.Contains(response.Body.String(), "response.completed") {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "text/event-stream" || response.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers = %#v", response.Header())
	}
}
