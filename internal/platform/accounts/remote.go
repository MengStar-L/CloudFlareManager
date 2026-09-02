package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RemoteBucket is an R2 bucket as it exists on Cloudflare, independent of
// whether it has been registered locally.
type RemoteBucket struct {
	Name         string `json:"name"`
	CreationDate string `json:"creation_date,omitempty"`
	Jurisdiction string `json:"jurisdiction"`
	Location     string `json:"location,omitempty"`
	StorageClass string `json:"storage_class,omitempty"`
}

// RemoteClient performs R2 management operations against the Cloudflare API
// on behalf of a stored account.
type RemoteClient struct {
	BaseURL string
	Client  *http.Client
}

const remoteBucketPageLimit = 10

// R2Buckets lists the R2 buckets that exist on Cloudflare for the account.
func (c RemoteClient) R2Buckets(ctx context.Context, cloudflareAccountID, apiToken string) ([]RemoteBucket, error) {
	return c.r2BucketsInJurisdiction(ctx, cloudflareAccountID, apiToken, "default")
}

// R2BucketsAllJurisdictions lists every jurisdiction separately so equal
// bucket names cannot be merged across data-residency boundaries.
func (c RemoteClient) R2BucketsAllJurisdictions(ctx context.Context, cloudflareAccountID, apiToken string) ([]RemoteBucket, error) {
	jurisdictions := []string{"default", "eu", "us", "fedramp"}
	var buckets []RemoteBucket
	for _, jurisdiction := range jurisdictions {
		items, err := c.r2BucketsInJurisdiction(ctx, cloudflareAccountID, apiToken, jurisdiction)
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, items...)
	}
	return buckets, nil
}

func (c RemoteClient) r2BucketsInJurisdiction(ctx context.Context, cloudflareAccountID, apiToken, jurisdiction string) ([]RemoteBucket, error) {
	if cloudflareAccountID == "" || apiToken == "" {
		return nil, fmt.Errorf("account ID and API token are required")
	}
	if jurisdiction == "" {
		jurisdiction = "default"
	}

	var buckets []RemoteBucket
	cursor := ""
	for page := 0; page < remoteBucketPageLimit; page++ {
		path := "/accounts/" + url.PathEscape(cloudflareAccountID) + "/r2/buckets?per_page=100"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		envelope, err := c.doRemoteR2Request(ctx, http.MethodGet, path, apiToken, jurisdiction, "list R2 buckets")
		if err != nil {
			return nil, err
		}
		pageBuckets, err := decodeRemoteBuckets(envelope.Result)
		if err != nil {
			return nil, err
		}
		for index := range pageBuckets {
			if pageBuckets[index].Jurisdiction == "" {
				pageBuckets[index].Jurisdiction = jurisdiction
			}
		}
		buckets = append(buckets, pageBuckets...)
		var info struct {
			Cursor string `json:"cursor"`
		}
		if err := decodeCloudflareResult("list R2 buckets pagination", envelope.ResultInfo, &info); err != nil {
			return nil, err
		}
		cursor = info.Cursor
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
	if envelope.Result.Jurisdiction == "" {
		envelope.Result.Jurisdiction = "default"
	}
	return envelope.Result, nil
}

// RemoteObject is an object returned by Cloudflare's R2 management API.
type RemoteObject struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

// RemoteObjectPage contains one cursor-based page of R2 objects.
type RemoteObjectPage struct {
	Objects   []RemoteObject `json:"objects"`
	Cursor    string         `json:"cursor,omitempty"`
	Truncated bool           `json:"truncated"`
}

// GetR2Bucket returns the remote identity and placement metadata of a bucket.
func (c RemoteClient) GetR2Bucket(ctx context.Context, accountID, token, jurisdiction, bucket string) (RemoteBucket, error) {
	jurisdiction, err := validateRemoteR2Arguments(accountID, token, jurisdiction, bucket)
	if err != nil {
		return RemoteBucket{}, err
	}
	const operation = "get R2 bucket"
	envelope, err := c.doRemoteR2Request(ctx, http.MethodGet,
		remoteR2BucketPath(accountID, bucket), token, jurisdiction, operation)
	if err != nil {
		return RemoteBucket{}, err
	}
	var result RemoteBucket
	if err := decodeCloudflareResult(operation, envelope.Result, &result); err != nil {
		return RemoteBucket{}, err
	}
	if result.Name == "" {
		result.Name = bucket
	}
	if result.Jurisdiction == "" {
		result.Jurisdiction = jurisdiction
	}
	return result, nil
}

// ListR2Objects returns one page of objects. Pass the returned cursor to the
// next call; a limit outside Cloudflare's 1-1000 range is clamped.
func (c RemoteClient) ListR2Objects(ctx context.Context, accountID, token, jurisdiction, bucket, cursor string, limit int) (RemoteObjectPage, error) {
	jurisdiction, err := validateRemoteR2Arguments(accountID, token, jurisdiction, bucket)
	if err != nil {
		return RemoteObjectPage{}, err
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}
	query := url.Values{"per_page": []string{strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	const operation = "list R2 objects"
	envelope, err := c.doRemoteR2Request(ctx, http.MethodGet,
		remoteR2BucketPath(accountID, bucket)+"/objects?"+query.Encode(), token, jurisdiction, operation)
	if err != nil {
		return RemoteObjectPage{}, err
	}
	var objects []RemoteObject
	if err := decodeCloudflareResult(operation, envelope.Result, &objects); err != nil {
		return RemoteObjectPage{}, err
	}
	var info struct {
		Cursor      string `json:"cursor"`
		IsTruncated bool   `json:"is_truncated"`
	}
	if err := decodeCloudflareResult(operation+" pagination", envelope.ResultInfo, &info); err != nil {
		return RemoteObjectPage{}, err
	}
	return RemoteObjectPage{
		Objects: objects, Cursor: info.Cursor, Truncated: info.IsTruncated || info.Cursor != "",
	}, nil
}

// DeleteR2Object deletes one object. Slashes in object keys remain literal as
// required by Cloudflare; every other key segment is path escaped separately.
func (c RemoteClient) DeleteR2Object(ctx context.Context, accountID, token, jurisdiction, bucket, key string) error {
	jurisdiction, err := validateRemoteR2Arguments(accountID, token, jurisdiction, bucket)
	if err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("object key is required")
	}
	const operation = "delete R2 object"
	_, err = c.doRemoteR2Request(ctx, http.MethodDelete,
		remoteR2BucketPath(accountID, bucket)+"/objects/"+escapeR2ObjectKey(key), token, jurisdiction, operation)
	return err
}

// DeleteR2Bucket permanently deletes an empty bucket on Cloudflare.
func (c RemoteClient) DeleteR2Bucket(ctx context.Context, accountID, token, jurisdiction, bucket string) error {
	jurisdiction, err := validateRemoteR2Arguments(accountID, token, jurisdiction, bucket)
	if err != nil {
		return err
	}
	const operation = "delete R2 bucket"
	_, err = c.doRemoteR2Request(ctx, http.MethodDelete,
		remoteR2BucketPath(accountID, bucket), token, jurisdiction, operation)
	return err
}

func validateRemoteR2Arguments(accountID, token, jurisdiction, bucket string) (string, error) {
	if accountID == "" || token == "" {
		return "", fmt.Errorf("account ID and API token are required")
	}
	if bucket == "" {
		return "", fmt.Errorf("bucket name is required")
	}
	if jurisdiction == "" {
		jurisdiction = "default"
	}
	return jurisdiction, nil
}

func (c RemoteClient) doRemoteR2Request(ctx context.Context, method, path, token, jurisdiction, operation string) (cloudflareEnvelope, error) {
	baseURL := strings.TrimRight(c.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4"
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, nil)
	if err != nil {
		return cloudflareEnvelope{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("cf-r2-jurisdiction", jurisdiction)
	response, err := client.Do(request)
	if err != nil {
		return cloudflareEnvelope{}, fmt.Errorf("%s: %w", operation, err)
	}
	return readCloudflareEnvelope(operation, response, token)
}

func remoteR2BucketPath(accountID, bucket string) string {
	return "/accounts/" + url.PathEscape(accountID) + "/r2/buckets/" + url.PathEscape(bucket)
}

func escapeR2ObjectKey(key string) string {
	segments := strings.Split(key, "/")
	for index := range segments {
		segments[index] = url.PathEscape(segments[index])
	}
	return strings.Join(segments, "/")
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
