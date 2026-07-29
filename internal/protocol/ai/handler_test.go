package aiprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	aimodule "github.com/cf-r2-manager/cf-r2-manager/internal/modules/ai"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestModelsReturnsOpenAIModelList(t *testing.T) {
	t.Parallel()

	handler := Handler{
		Verify: validVerifier,
		Models: func(context.Context) ([]map[string]any, error) {
			return []map[string]any{
				{"name": "@cf/zeta"}, {"id": "@cf/alpha"}, {"name": "@cf/zeta"}, {"name": ""},
			}, nil
		},
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer cfai_test.secret")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Object != "list" || len(payload.Data) != 2 {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Data[0].ID != "@cf/alpha" || payload.Data[1].ID != "@cf/zeta" {
		t.Fatalf("models = %#v", payload.Data)
	}
	for _, model := range payload.Data {
		if model.Object != "model" || model.Created != 0 || model.OwnedBy != "cloudflare" {
			t.Fatalf("model = %#v", model)
		}
	}
}

func TestModelsRequiresValidAICredential(t *testing.T) {
	t.Parallel()

	called := false
	handler := Handler{
		Verify: func(context.Context, string, string) (Identity, error) {
			return Identity{}, errors.New("invalid")
		},
		Models: func(context.Context) ([]map[string]any, error) {
			called = true
			return nil, nil
		},
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer cfai_test.wrong")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized || called {
		t.Fatalf("status = %d, catalog called = %v", response.Code, called)
	}
}

func TestModelsReportsCatalogFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "no account", err: aimodule.ErrNoAICapableAccount, want: http.StatusServiceUnavailable},
		{name: "upstream", err: errors.New("cloudflare unavailable"), want: http.StatusBadGateway},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler := Handler{
				Verify: validVerifier,
				Models: func(context.Context) ([]map[string]any, error) { return nil, test.err },
			}
			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			request.Header.Set("Authorization", "Bearer cfai_test.secret")
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.want {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestInferenceReturnsOpenAIErrorForBlockedModel(t *testing.T) {
	t.Parallel()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{21}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := accountStore.SetCapabilities(context.Background(), account.ID, []accounts.Capability{{Name: "ai", Available: true, CheckedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err := accountStore.SetHealth(context.Background(), account.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}
	handler := Handler{
		Verify: validVerifier,
		Gateway: &aimodule.Gateway{
			Accounts: accountStore, DB: db, Policy: aimodule.NewModelPolicy(db), NeuronSoftLimit: 9_000,
		},
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"@cf/zai-org/glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer cfai_test.secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Type != "model_not_available" || payload.Error.Code != "model_not_available" {
		t.Fatalf("error = %#v", payload.Error)
	}
}

func validVerifier(context.Context, string, string) (Identity, error) {
	return Identity{ID: "credential", Scopes: []string{"ai:invoke"}}, nil
}
