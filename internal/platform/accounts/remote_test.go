package accounts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemoteClientListsBucketsAcrossPages(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success":     true,
				"result":      map[string]any{"buckets": []map[string]string{{"name": "alpha"}, {"name": "beta"}}},
				"result_info": map[string]any{"cursor": "next"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result":  map[string]any{"buckets": []map[string]string{{"name": "gamma"}}},
		})
	}))
	defer server.Close()

	client := RemoteClient{BaseURL: server.URL, Client: server.Client()}
	buckets, err := client.R2Buckets(context.Background(), "account", "token")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 3 || buckets[0].Name != "alpha" || buckets[2].Name != "gamma" {
		t.Fatalf("buckets = %#v", buckets)
	}
}

func TestRemoteClientAcceptsBareArrayResult(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result":  []map[string]string{{"name": "solo"}},
		})
	}))
	defer server.Close()

	client := RemoteClient{BaseURL: server.URL, Client: server.Client()}
	buckets, err := client.R2Buckets(context.Background(), "account", "token")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || buckets[0].Name != "solo" {
		t.Fatalf("buckets = %#v", buckets)
	}
}
