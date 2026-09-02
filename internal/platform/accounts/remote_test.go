package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	for _, bucket := range buckets {
		if bucket.Jurisdiction != "default" {
			t.Fatalf("bucket jurisdiction = %q", bucket.Jurisdiction)
		}
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

func TestRemoteClientListsBucketsAcrossJurisdictions(t *testing.T) {
	t.Parallel()

	seen := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jurisdiction := r.Header.Get("cf-r2-jurisdiction")
		seen[jurisdiction]++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{"buckets": []map[string]string{{
				"name": "same-name", "jurisdiction": jurisdiction,
			}}},
		})
	}))
	defer server.Close()

	client := RemoteClient{BaseURL: server.URL, Client: server.Client()}
	buckets, err := client.R2BucketsAllJurisdictions(context.Background(), "account", "token")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 4 {
		t.Fatalf("buckets = %#v", buckets)
	}
	for _, jurisdiction := range []string{"default", "eu", "us", "fedramp"} {
		if seen[jurisdiction] != 1 {
			t.Fatalf("jurisdiction %q requests = %d", jurisdiction, seen[jurisdiction])
		}
	}
}

func TestRemoteClientGetsBucketIdentity(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %q", r.Method)
		}
		if r.URL.Path != "/accounts/account/r2/buckets/my-bucket" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		assertR2RequestHeaders(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"name":          "my-bucket",
				"creation_date": "2026-08-30T10:11:12.000Z",
				"location":      "APAC",
				"storage_class": "Standard",
			},
		})
	}))
	defer server.Close()

	client := RemoteClient{BaseURL: server.URL, Client: server.Client()}
	bucket, err := client.GetR2Bucket(context.Background(), "account", "token", "default", "my-bucket")
	if err != nil {
		t.Fatal(err)
	}
	if bucket.Name != "my-bucket" || bucket.CreationDate != "2026-08-30T10:11:12.000Z" {
		t.Fatalf("bucket = %#v", bucket)
	}
	if bucket.Jurisdiction != "default" || bucket.Location != "APAC" || bucket.StorageClass != "Standard" {
		t.Fatalf("bucket metadata = %#v", bucket)
	}
}

func TestRemoteClientListsObjectsAcrossPages(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %q", r.Method)
		}
		if r.URL.Path != "/accounts/account/r2/buckets/my-bucket/objects" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		assertR2RequestHeaders(t, r)
		if r.URL.Query().Get("per_page") != "2" {
			t.Fatalf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  []map[string]any{{"key": "one", "size": 11}, {"key": "two", "size": 22}},
				"result_info": map[string]any{
					"cursor":       "next cursor",
					"is_truncated": true,
				},
			})
			return
		}
		if r.URL.Query().Get("cursor") != "next cursor" {
			t.Fatalf("cursor = %q", r.URL.Query().Get("cursor"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result":  []map[string]any{{"key": "three", "size": 33}},
			"result_info": map[string]any{
				"is_truncated": false,
			},
		})
	}))
	defer server.Close()

	client := RemoteClient{BaseURL: server.URL, Client: server.Client()}
	first, err := client.ListR2Objects(context.Background(), "account", "token", "default", "my-bucket", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Objects) != 2 || first.Objects[1].Key != "two" || first.Cursor != "next cursor" || !first.Truncated {
		t.Fatalf("first page = %#v", first)
	}
	second, err := client.ListR2Objects(context.Background(), "account", "token", "default", "my-bucket", first.Cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Objects) != 1 || second.Objects[0].Size != 33 || second.Cursor != "" || second.Truncated {
		t.Fatalf("second page = %#v", second)
	}
}

func TestRemoteClientDeletesObjectWithLiteralSlashes(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %q", r.Method)
		}
		const wantPath = "/accounts/account/r2/buckets/my-bucket/objects/folder/nested/file%20name%3F%23.txt"
		if r.URL.EscapedPath() != wantPath {
			t.Fatalf("escaped path = %q, want %q", r.URL.EscapedPath(), wantPath)
		}
		if strings.Contains(strings.ToLower(r.RequestURI), "%2f") {
			t.Fatalf("object slashes were escaped in %q", r.RequestURI)
		}
		assertR2RequestHeaders(t, r)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := RemoteClient{BaseURL: server.URL, Client: server.Client()}
	if err := client.DeleteR2Object(context.Background(), "account", "token", "default", "my-bucket", "folder/nested/file name?#.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteClientDeletesBucket(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %q", r.Method)
		}
		if r.URL.Path != "/accounts/account/r2/buckets/my-bucket" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		assertR2RequestHeaders(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
	}))
	defer server.Close()

	client := RemoteClient{BaseURL: server.URL, Client: server.Client()}
	if err := client.DeleteR2Bucket(context.Background(), "account", "token", "default", "my-bucket"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteClientClassifiesCloudflareErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		code       int
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized, code: 9109},
		{name: "forbidden", statusCode: http.StatusForbidden, code: 10000},
		{name: "not found", statusCode: http.StatusNotFound, code: 10006},
		{name: "bucket not empty", statusCode: http.StatusConflict, code: 10008},
		{name: "rate limited", statusCode: http.StatusTooManyRequests, code: 1015},
		{name: "service unavailable", statusCode: http.StatusServiceUnavailable, code: 10001},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.statusCode)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": false,
					"errors":  []map[string]any{{"code": test.code, "message": "rejected"}},
				})
			}))
			defer server.Close()

			client := RemoteClient{BaseURL: server.URL, Client: server.Client()}
			err := client.DeleteR2Bucket(context.Background(), "account", "token", "default", "my-bucket")
			var apiErr *CloudflareAPIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %T %v", err, err)
			}
			if apiErr.StatusCode != test.statusCode || apiErr.Code != test.code || apiErr.Message != "rejected" {
				t.Fatalf("api error = %#v", apiErr)
			}
			if apiErr.Operation != "delete R2 bucket" {
				t.Fatalf("operation = %q", apiErr.Operation)
			}
			if strings.Contains(err.Error(), "token") {
				t.Fatalf("error leaks API token: %q", err)
			}
		})
	}
}

func TestRemoteClientRejectsSuccessFalse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"errors":  []map[string]any{{"code": 10008, "message": "bucket is not empty"}},
		})
	}))
	defer server.Close()

	client := RemoteClient{BaseURL: server.URL, Client: server.Client()}
	err := client.DeleteR2Bucket(context.Background(), "account", "token", "default", "my-bucket")
	var apiErr *CloudflareAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if apiErr.StatusCode != http.StatusOK || apiErr.Code != 10008 {
		t.Fatalf("api error = %#v", apiErr)
	}
}

func TestRemoteClientHandlesNonJSONError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream HTML was not JSON"))
	}))
	defer server.Close()

	client := RemoteClient{BaseURL: server.URL, Client: server.Client()}
	err := client.DeleteR2Bucket(context.Background(), "account", "token", "default", "my-bucket")
	var apiErr *CloudflareAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway || apiErr.Code != 0 || apiErr.Message == "" {
		t.Fatalf("api error = %#v", apiErr)
	}
}

func TestRemoteClientRedactsTokenFromCloudflareError(t *testing.T) {
	t.Parallel()

	const token = "secret-token-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"errors":  []map[string]any{{"code": 10000, "message": "rejected " + token}},
		})
	}))
	defer server.Close()

	client := RemoteClient{BaseURL: server.URL, Client: server.Client()}
	err := client.DeleteR2Bucket(context.Background(), "account", token, "default", "my-bucket")
	var apiErr *CloudflareAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if strings.Contains(apiErr.Message, token) || strings.Contains(err.Error(), token) {
		t.Fatalf("error leaks API token: %#v", apiErr)
	}
}

func TestR2NotFoundClassificationRequiresStructuredErrorCode(t *testing.T) {
	t.Parallel()
	bucketMissing := &CloudflareAPIError{Operation: "get R2 bucket", StatusCode: http.StatusNotFound, Code: 10006}
	objectMissing := &CloudflareAPIError{Operation: "delete R2 object", StatusCode: http.StatusNotFound, Code: 10007}
	proxyMissing := &CloudflareAPIError{Operation: "get R2 bucket", StatusCode: http.StatusNotFound}
	if !IsR2BucketNotFound(bucketMissing) || IsR2BucketNotFound(objectMissing) || IsR2BucketNotFound(proxyMissing) {
		t.Fatal("bucket not-found classification accepted an ambiguous 404")
	}
	if !IsR2ObjectNotFound(objectMissing) || IsR2ObjectNotFound(bucketMissing) || IsR2ObjectNotFound(proxyMissing) {
		t.Fatal("object not-found classification accepted an ambiguous 404")
	}
}

func assertR2RequestHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Header.Get("Authorization") != "Bearer token" {
		t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
	}
	if r.Header.Get("cf-r2-jurisdiction") != "default" {
		t.Fatalf("jurisdiction header = %q", r.Header.Get("cf-r2-jurisdiction"))
	}
}
