package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
)

type Management struct {
	Accounts   *accounts.Store
	BaseURL    string
	HTTPClient *http.Client
}

func (m Management) ListModels(ctx context.Context, accountID string) ([]map[string]any, error) {
	return managementCall[[]map[string]any](ctx, m, accountID, http.MethodGet, "/ai/models/search?per_page=100", nil)
}

func (m Management) ListGateways(ctx context.Context, accountID string) ([]map[string]any, error) {
	return managementCall[[]map[string]any](ctx, m, accountID, http.MethodGet, "/ai-gateway/gateways", nil)
}

func (m Management) CreateGateway(ctx context.Context, accountID string, input map[string]any) (map[string]any, error) {
	return managementCall[map[string]any](ctx, m, accountID, http.MethodPost, "/ai-gateway/gateways", input)
}

func (m Management) DeleteGateway(ctx context.Context, accountID, gatewayID string) error {
	_, err := managementCall[json.RawMessage](ctx, m, accountID, http.MethodDelete, "/ai-gateway/gateways/"+url.PathEscape(gatewayID), nil)
	return err
}

func (m Management) GatewayLogs(ctx context.Context, accountID, gatewayID string, limit int) (json.RawMessage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return managementCall[json.RawMessage](ctx, m, accountID, http.MethodGet,
		fmt.Sprintf("/ai-gateway/gateways/%s/logs?per_page=%d", url.PathEscape(gatewayID), limit), nil)
}

func managementCall[T any](ctx context.Context, management Management, accountID, method, suffix string, body any) (T, error) {
	var zero T
	if management.Accounts == nil {
		return zero, errors.New("AI Gateway management is not configured")
	}
	account, err := management.Accounts.Get(ctx, accountID, true)
	if err != nil {
		return zero, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return zero, err
		}
		reader = bytes.NewReader(encoded)
	}
	baseURL := strings.TrimRight(management.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4"
	}
	request, err := http.NewRequestWithContext(ctx, method, baseURL+"/accounts/"+url.PathEscape(account.CloudflareAccountID)+suffix, reader)
	if err != nil {
		return zero, err
	}
	request.Header.Set("Authorization", "Bearer "+account.APIToken)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := management.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return zero, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return zero, err
	}
	var envelope struct {
		Success bool `json:"success"`
		Result  T    `json:"result"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return zero, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.Success {
		message := fmt.Sprintf("Cloudflare AI Gateway returned HTTP %d", response.StatusCode)
		if len(envelope.Errors) > 0 && envelope.Errors[0].Message != "" {
			message = envelope.Errors[0].Message
		}
		return zero, errors.New(message)
	}
	return envelope.Result, nil
}
