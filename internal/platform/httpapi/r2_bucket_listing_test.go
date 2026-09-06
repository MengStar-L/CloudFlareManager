package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
)

func TestRemoteBucketListsKeepPartialResults(t *testing.T) {
	t.Parallel()
	for _, failed := range []string{"fedramp", "default", "all"} {
		t.Run(failed, func(t *testing.T) {
			fixture := newR2DeletionAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/graphql" {
					_, _ = w.Write([]byte(`{"data":{"viewer":{"accounts":[{"r2StorageAdaptiveGroups":[{"dimensions":{"bucketName":"remote"},"max":{"payloadSize":99,"metadataSize":5,"objectCount":7}}]}]}}}`))
					return
				}
				jurisdiction := r.Header.Get("cf-r2-jurisdiction")
				if failed == "all" || jurisdiction == failed {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10003,"message":"Access Denied"}]}`))
					return
				}
				buckets := []map[string]string{}
				if jurisdiction == "default" || jurisdiction == "eu" {
					buckets = append(buckets, map[string]string{"name": "remote"})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"buckets": buckets}})
			})
			_, err := fixture.index.CreateBucket(context.Background(), r2.CreateBucketInput{AccountID: fixture.account.ID, Name: "local"})
			if err != nil {
				t.Fatal(err)
			}
			response := fixture.request(t, http.MethodGet, "/api/v1/r2/remote-buckets?account_id="+fixture.account.ID, nil)
			if failed == "all" {
				if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "所有管辖区") {
					t.Fatalf("response = %d %s", response.Code, response.Body)
				}
			} else {
				if response.Code != http.StatusOK {
					t.Fatalf("response = %d %s", response.Code, response.Body)
				}
				var result remoteBucketListView
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if len(result.Warnings) != 1 || result.Warnings[0].Jurisdiction != failed {
					t.Fatalf("warnings = %#v", result.Warnings)
				}
				localFound, euFound := false, false
				for _, bucket := range result.Buckets {
					if bucket.Name == "local" {
						localFound = true
						if !bucket.Managed || bucket.RemoteMissing != (failed != "default") || bucket.RemoteUnknown != (failed == "default") {
							t.Fatalf("local state = %#v", bucket)
						}
					}
					if bucket.Jurisdiction == "eu" && bucket.Name == "remote" {
						euFound = true
					}
				}
				if !localFound || !euFound {
					t.Fatalf("buckets lost: %#v", result.Buckets)
				}
				if failed == "default" {
					if result.Usage["total_bytes"] != nil || result.Usage["remaining_bytes"] != nil || result.Usage["usage_error"] == nil {
						t.Fatalf("incorrect partial totals: %#v", result.Usage)
					}
				} else if result.Usage["total_bytes"] != float64(99) {
					t.Fatalf("default stats lost: %#v", result.Usage)
				}
			}
			overview := fixture.request(t, http.MethodGet, "/api/v1/r2/overview", nil)
			var payload struct {
				Accounts []struct {
					remoteBucketListView
					Error string `json:"error"`
				} `json:"accounts"`
			}
			if err := json.Unmarshal(overview.Body.Bytes(), &payload); err != nil || len(payload.Accounts) != 1 {
				t.Fatalf("overview = %s, error = %v", overview.Body, err)
			}
			entry := payload.Accounts[0]
			if failed == "all" {
				if !strings.Contains(entry.Error, "所有管辖区") {
					t.Fatalf("overview hid failure: %s", overview.Body)
				}
			} else if entry.Error != "" || len(entry.Warnings) != 1 || len(entry.Buckets) == 0 {
				t.Fatalf("overview lost partial result: %s", overview.Body)
			}
		})
	}
}
