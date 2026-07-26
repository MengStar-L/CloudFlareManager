package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		available, detail, err := runProbe(ctx, client, baseURL, apiToken, probe.method, probe.path, probe.body)
		if err != nil {
			return nil, err
		}
		if !available && probe.fallbackPath != "" {
			fallbackAvailable, _, fallbackErr := runProbe(ctx, client, baseURL, apiToken, probe.method, probe.fallbackPath, probe.body)
			if fallbackErr != nil {
				return nil, fallbackErr
			}
			if fallbackAvailable {
				available, detail = true, ""
			}
		}
		capabilities = append(capabilities, Capability{
			Name: probe.name, Available: available, Detail: detail, CheckedAt: time.Now(),
		})
	}
	return capabilities, nil
}

func runProbe(ctx context.Context, client *http.Client, baseURL, token, method, path string, body []byte) (bool, string, error) {
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return false, "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if len(body) != 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return false, "", fmt.Errorf("cloudflare capability probe: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if err != nil {
		return false, "", err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		var envelope struct {
			Success *bool `json:"success"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(data, &envelope); err == nil {
			if envelope.Success != nil && !*envelope.Success {
				return false, firstCloudflareError(envelope.Errors), nil
			}
			if len(envelope.Errors) != 0 {
				return false, firstCloudflareError(envelope.Errors), nil
			}
		}
		return true, "", nil
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return false, http.StatusText(response.StatusCode), nil
	}
	return false, fmt.Sprintf("Cloudflare returned HTTP %d", response.StatusCode), nil
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
