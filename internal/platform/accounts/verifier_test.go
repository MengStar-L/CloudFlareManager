package accounts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifierDetectsCapabilities(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/graphql" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"viewer": map[string]any{}}})
			return
		}
		switch r.URL.Path {
		case "/user/tokens/verify", "/accounts/account/r2/buckets", "/accounts/account/d1/database", "/accounts/account/ai/models/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	verifier := Verifier{BaseURL: server.URL, Client: server.Client()}
	capabilities, err := verifier.Detect(context.Background(), "account", "token")
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 5 {
		t.Fatalf("capability count = %d", len(capabilities))
	}
	for _, capability := range capabilities {
		if !capability.Available {
			t.Fatalf("capability unavailable: %#v", capability)
		}
	}
}

// Account-owned API tokens are rejected by /user/tokens/verify; the verifier
// must fall back to the account-scoped verify endpoint.
func TestVerifierAcceptsAccountOwnedToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/tokens/verify":
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		case "/accounts/account/tokens/verify":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"status": "active"}})
		case "/graphql":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"viewer": map[string]any{}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
		}
	}))
	defer server.Close()

	verifier := Verifier{BaseURL: server.URL, Client: server.Client()}
	capabilities, err := verifier.Detect(context.Background(), "account", "token")
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range capabilities {
		if capability.Name == "api_token" && !capability.Available {
			t.Fatalf("api_token should be available via account-scoped verify: %#v", capability)
		}
	}
}
