package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Verifier struct {
	BaseURL string
	Client  *http.Client
}

func (v Verifier) Detect(ctx context.Context, accountID, apiToken string) ([]Capability, error) {
	if accountID == "" || apiToken == "" {
		return nil, fmt.Errorf("account ID and API token are required")
	}
	baseURL := strings.TrimRight(v.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4"
	}
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	probes := []struct {
		name         string
		method       string
		path         string
		fallbackPath string
		body         []byte
	}{
		// Account-owned API tokens cannot call /user/tokens/verify; they verify
		// through the account-scoped endpoint instead, so try both.
		{name: "api_token", method: http.MethodGet, path: "/user/tokens/verify", fallbackPath: "/accounts/" + accountID + "/tokens/verify"},
		{name: "r2", method: http.MethodGet, path: "/accounts/" + accountID + "/r2/buckets?per_page=1"},
		{name: "d1", method: http.MethodGet, path: "/accounts/" + accountID + "/d1/database?per_page=1"},
		{name: "ai", method: http.MethodGet, path: "/accounts/" + accountID + "/ai/models/search?per_page=1"},
		{name: "analytics", method: http.MethodPost, path: "/graphql", body: graphqlProbe(accountID)},
	}
	capabilities := make([]Capability, 0, len(probes))
	for _, probe := range probes {
		available, detail, err := runProbe(ctx, client, baseURL, apiToken, probe.name, probe.method, probe.path, probe.body)
		if err != nil {
			return nil, err
		}
		if !available && probe.fallbackPath != "" {
			fallbackAvailable, fallbackDetail, fallbackErr := runProbe(ctx, client, baseURL, apiToken, probe.name, probe.method, probe.fallbackPath, probe.body)
			if fallbackErr != nil {
				return nil, fmt.Errorf("%s\n%w", detail, fallbackErr)
			}
			if fallbackAvailable {
				available, detail = true, ""
			} else {
				detail += "\n" + fallbackDetail
			}
		}
		capabilities = append(capabilities, Capability{
			Name: probe.name, Available: available, Detail: detail, CheckedAt: time.Now(),
		})
	}
	return capabilities, nil
}

func runProbe(ctx context.Context, client *http.Client, baseURL, token, name, method, path string, body []byte) (bool, string, error) {
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return false, "", fmt.Errorf("%s：无法创建检测请求。%s %s；%s", capabilityLabel(name), method, path, redactProbeError(err.Error(), token))
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if len(body) != 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return false, "", fmt.Errorf("%s：无法连接 Cloudflare，尚不能判断凭证是否有效。请检查服务器网络、DNS、代理和 TLS 配置后重试。\n%s %s；%s", capabilityLabel(name), method, path, redactProbeError(err.Error(), token))
	}
	envelope, err := readCloudflareEnvelope(method+" "+path, response, token)
	if err != nil {
		var apiErr *CloudflareAPIError
		if errors.As(err, &apiErr) {
			return false, capabilityFailure(name, method, path, apiErr), nil
		}
		return false, "", fmt.Errorf("%s：无法读取或解析 Cloudflare 响应，检测结果无法确认。请检查服务器网络或代理后重试。\nHTTP %d；%s", capabilityLabel(name), response.StatusCode, redactProbeError(err.Error(), token))
	}
	if name == "api_token" {
		var result struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(envelope.Result, &result) == nil && result.Status != "" && result.Status != "active" {
			return false, capabilityFailure(name, method, path, &CloudflareAPIError{
				StatusCode: response.StatusCode,
				Message:    "Token status: " + redactProbeError(result.Status, token),
			}), nil
		}
	}
	return true, "", nil
}

func firstCloudflareError(errors []struct {
	Message string `json:"message"`
}) string {
	if len(errors) == 0 || errors[0].Message == "" {
		return "Cloudflare rejected the capability probe"
	}
	return errors[0].Message
}

func graphqlProbe(accountID string) []byte {
	body, _ := json.Marshal(map[string]any{
		"query":     "query CapabilityProbe($accountTag: String!) { viewer { accounts(filter: {accountTag: $accountTag}) { __typename } } }",
		"variables": map[string]string{"accountTag": accountID},
	})
	return body
}
