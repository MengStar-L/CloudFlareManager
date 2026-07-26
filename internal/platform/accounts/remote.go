package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RemoteBucket is an R2 bucket as it exists on Cloudflare, independent of
// whether it has been registered locally.
type RemoteBucket struct {
	Name         string `json:"name"`
	CreationDate string `json:"creation_date,omitempty"`
}

// RemoteClient performs read-only lookups against the Cloudflare API on
// behalf of a stored account.
type RemoteClient struct {
	BaseURL string
	Client  *http.Client
}

const remoteBucketPageLimit = 10

// R2Buckets lists the R2 buckets that exist on Cloudflare for the account.
func (c RemoteClient) R2Buckets(ctx context.Context, cloudflareAccountID, apiToken string) ([]RemoteBucket, error) {
	if cloudflareAccountID == "" || apiToken == "" {
		return nil, fmt.Errorf("account ID and API token are required")
	}
	baseURL := strings.TrimRight(c.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4"
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	var buckets []RemoteBucket
	cursor := ""
	for page := 0; page < remoteBucketPageLimit; page++ {
		endpoint := baseURL + "/accounts/" + url.PathEscape(cloudflareAccountID) + "/r2/buckets?per_page=100"
		if cursor != "" {
			endpoint += "&cursor=" + url.QueryEscape(cursor)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+apiToken)
		request.Header.Set("Accept", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("cloudflare bucket list: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, fmt.Errorf("cloudflare returned HTTP %d", response.StatusCode)
		}
		var envelope struct {
			Success *bool `json:"success"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
			Result     json.RawMessage `json:"result"`
			ResultInfo struct {
				Cursor string `json:"cursor"`
			} `json:"result_info"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return nil, fmt.Errorf("decode cloudflare bucket list: %w", err)
		}
		if envelope.Success != nil && !*envelope.Success {
			return nil, fmt.Errorf("cloudflare rejected the bucket list: %s", firstCloudflareError(envelope.Errors))
		}
		pageBuckets, err := decodeRemoteBuckets(envelope.Result)
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, pageBuckets...)
		cursor = envelope.ResultInfo.Cursor
		if cursor == "" || len(pageBuckets) == 0 {
			break
		}
	}
	return buckets, nil
}

// CreateR2Bucket creates a bucket on Cloudflare. Requires the token to hold
// Workers R2 Storage: Edit permission.
func (c RemoteClient) CreateR2Bucket(ctx context.Context, cloudflareAccountID, apiToken, name string) (RemoteBucket, error) {
	if cloudflareAccountID == "" || apiToken == "" {
		return RemoteBucket{}, fmt.Errorf("account ID and API token are required")
	}
	baseURL := strings.TrimRight(c.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4"
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	payload, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return RemoteBucket{}, err
	}
	endpoint := baseURL + "/accounts/" + url.PathEscape(cloudflareAccountID) + "/r2/buckets"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return RemoteBucket{}, err
	}
	request.Header.Set("Authorization", "Bearer "+apiToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return RemoteBucket{}, fmt.Errorf("cloudflare bucket create: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return RemoteBucket{}, err
	}
	var envelope struct {
		Success *bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Result RemoteBucket `json:"result"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return RemoteBucket{}, fmt.Errorf("cloudflare returned HTTP %d", response.StatusCode)
		}
		return RemoteBucket{}, fmt.Errorf("decode cloudflare bucket create: %w", err)
	}
	if (envelope.Success != nil && !*envelope.Success) || response.StatusCode < 200 || response.StatusCode >= 300 {
		return RemoteBucket{}, fmt.Errorf("cloudflare rejected the bucket create: %s", firstCloudflareError(envelope.Errors))
	}
	if envelope.Result.Name == "" {
		envelope.Result.Name = name
	}
	return envelope.Result, nil
}

// BucketUsage is the storage footprint of one bucket as reported by the
// Cloudflare analytics dataset backing the official dashboard.
type BucketUsage struct {
	PayloadBytes  int64 `json:"payload_bytes"`
	MetadataBytes int64 `json:"metadata_bytes"`
	ObjectCount   int64 `json:"object_count"`
}

// R2BucketUsage returns per-bucket storage usage via the GraphQL analytics
// API (dataset r2StorageAdaptiveGroups, same source as the dashboard).
func (c RemoteClient) R2BucketUsage(ctx context.Context, cloudflareAccountID, apiToken string) (map[string]BucketUsage, error) {
	if cloudflareAccountID == "" || apiToken == "" {
		return nil, fmt.Errorf("account ID and API token are required")
	}
	baseURL := strings.TrimRight(c.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4"
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	since := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	query := fmt.Sprintf(`{ viewer { accounts(filter: {accountTag: %q}) { r2StorageAdaptiveGroups(limit: 500, filter: {datetime_geq: %q}) { dimensions { bucketName } max { payloadSize metadataSize objectCount } } } } }`, cloudflareAccountID, since)
	payload, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/graphql", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+apiToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("cloudflare usage query: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("cloudflare returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Data struct {
			Viewer struct {
				Accounts []struct {
					Groups []struct {
						Dimensions struct {
							BucketName string `json:"bucketName"`
						} `json:"dimensions"`
						Max struct {
							PayloadSize  float64 `json:"payloadSize"`
							MetadataSize float64 `json:"metadataSize"`
							ObjectCount  float64 `json:"objectCount"`
						} `json:"max"`
					} `json:"r2StorageAdaptiveGroups"`
				} `json:"accounts"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("decode cloudflare usage: %w", err)
	}
	if len(envelope.Errors) != 0 {
		return nil, fmt.Errorf("cloudflare usage query failed: %s", envelope.Errors[0].Message)
	}
	usage := make(map[string]BucketUsage)
	for _, account := range envelope.Data.Viewer.Accounts {
		for _, group := range account.Groups {
			name := group.Dimensions.BucketName
			if name == "" {
				continue
			}
			current := usage[name]
			if int64(group.Max.PayloadSize) >= current.PayloadBytes {
				usage[name] = BucketUsage{
					PayloadBytes:  int64(group.Max.PayloadSize),
					MetadataBytes: int64(group.Max.MetadataSize),
					ObjectCount:   int64(group.Max.ObjectCount),
				}
			}
		}
	}
	return usage, nil
}

// decodeRemoteBuckets accepts both response shapes Cloudflare has used for
// the R2 bucket list: {"buckets": [...]} and a bare array.
func decodeRemoteBuckets(result json.RawMessage) ([]RemoteBucket, error) {
	if len(result) == 0 {
		return nil, nil
	}
	var wrapped struct {
		Buckets []RemoteBucket `json:"buckets"`
	}
	if err := json.Unmarshal(result, &wrapped); err == nil && wrapped.Buckets != nil {
		return wrapped.Buckets, nil
	}
	var bare []RemoteBucket
	if err := json.Unmarshal(result, &bare); err == nil {
		return bare, nil
	}
	return nil, fmt.Errorf("unrecognized cloudflare bucket list format")
}
