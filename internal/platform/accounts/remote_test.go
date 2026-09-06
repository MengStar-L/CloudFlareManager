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
	result, err := client.R2BucketsAllJurisdictions(context.Background(), "account", "token")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Buckets) != 4 || len(result.Warnings) != 0 || len(result.SuccessfulJurisdictions) != 4 {
		t.Fatalf("bucket list = %#v", result)
	}
	for _, jurisdiction := range []string{"default", "eu", "us", "fedramp"} {
		if seen[jurisdiction] != 1 {
			t.Fatalf("jurisdiction %q requests = %d", jurisdiction, seen[jurisdiction])
		}
	}
}

func TestRemoteClientPreservesSuccessfulJurisdictions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, failed       string
		empty, pageFailure bool
	}{
		{name: "FedRAMP denied", failed: "fedramp"},
		{name: "default denied", failed: "default"},
		{name: "empty successful regions", failed: "fedramp", empty: true},
		{name: "all denied", failed: "all"},
		{name: "later page fails", failed: "default", pageFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			seen := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				jurisdiction := r.Header.Get("cf-r2-jurisdiction")
				seen[jurisdiction]++
				if test.failed == "all" || jurisdiction == test.failed {
					if test.pageFailure && r.URL.Query().Get("cursor") == "" {
						_, _ = w.Write([]byte(`{"success":true,"result":{"buckets":[{"name":"partial"}]},"result_info":{"cursor":"next"}}`))
						return
					}
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10003,"message":"Access Denied secret-token"}]}`))
					return
				}
				buckets := []map[string]string{}
				if !test.empty {
					buckets = append(buckets, map[string]string{"name": "same-name"})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"buckets": buckets}})
			}))
			defer server.Close()
			result, err := (RemoteClient{BaseURL: server.URL, Client: server.Client()}).R2BucketsAllJurisdictions(context.Background(), "account", "secret-token")
			if (err != nil) != (test.failed == "all") {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
			wantSuccess, wantWarnings := 3, 1
			if test.failed == "all" {
				wantSuccess, wantWarnings = 0, 4
			}
			if len(seen) != 4 || len(result.SuccessfulJurisdictions) != wantSuccess || len(result.Warnings) != wantWarnings {
				t.Fatalf("result = %#v, queried = %#v", result, seen)
			}
			wantBuckets := wantSuccess
			if test.empty {
				wantBuckets = 0
			}
			if len(result.Buckets) != wantBuckets {
				t.Fatalf("buckets = %#v", result.Buckets)
			}
			for _, bucket := range result.Buckets {
				if bucket.Jurisdiction == test.failed || bucket.Name == "partial" {
					t.Fatalf("incomplete region accepted: %#v", bucket)
				}
			}
			for _, warning := range result.Warnings {
				if !strings.Contains(warning.Message, "HTTP 403") || !strings.Contains(warning.Message, "10003") || strings.Contains(warning.Message, "secret-token") {
					t.Fatalf("incorrect or unsafe warning: %#v", warning)
				}
				if warning.Jurisdiction == "fedramp" && !strings.Contains(warning.Message, "单独开通") {
					t.Fatalf("missing FedRAMP advice: %#v", warning)
				}
			}
			if err != nil && (strings.Contains(err.Error(), "secret-token") || !strings.Contains(err.Error(), "default")) {
				t.Fatalf("incorrect all-region failure: %v", err)
			}
		})
	}
}

func TestRemoteClientRejectsIncompletePagination(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"result":{"buckets":[{"name":"partial"}]},"result_info":{"cursor":"next"}}`))
	}))
	defer server.Close()
	buckets, err := (RemoteClient{BaseURL: server.URL, Client: server.Client()}).R2Buckets(context.Background(), "account", "token")
	if err == nil || !strings.Contains(err.Error(), "incomplete") || len(buckets) != 0 {
		t.Fatalf("incomplete listing = %#v, error = %v", buckets, err)
	}
}

func TestRemoteClientStopsCancelledJurisdictionQueries(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (RemoteClient{}).R2BucketsAllJurisdictions(ctx, "account", "token")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestRemoteClientDoesNotTreatMissingDataAsAnEmptyListing(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{"success":true}`, `{"success":true,"result":null}`, `{"success":true,"result":{"buckets":[]},"result_info":{"cursor":"next"}}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		_, err := (RemoteClient{BaseURL: server.URL, Client: server.Client()}).R2Buckets(context.Background(), "account", "token")
		server.Close()
		if err == nil {
			t.Errorf("incomplete response accepted: %s", body)
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
