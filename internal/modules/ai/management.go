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
	const pageSize = 100
	models := make([]map[string]any, 0, pageSize)
	var previousPage []byte
	for page := 1; page <= 1000; page++ {
		items, info, err := managementCallWithInfo[[]map[string]any](ctx, m, accountID, http.MethodGet,
			fmt.Sprintf("/ai/models/search?per_page=%d&page=%d", pageSize, page), nil)
		if err != nil {
			return nil, err
		}
		if info.Page > 0 && info.Page != page {
			return nil, fmt.Errorf("Cloudflare model catalog returned page %d for requested page %d", info.Page, page)
		}
		encodedPage, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		if page > 1 && bytes.Equal(encodedPage, previousPage) {
			return nil, fmt.Errorf("Cloudflare model catalog repeated page %d", page)
		}
		previousPage = encodedPage
		models = append(models, items...)
		effectivePageSize := pageSize
		if info.PerPage > 0 {
			effectivePageSize = info.PerPage
		}
		if info.TotalPages > 0 && page >= info.TotalPages || info.TotalPages == 0 && len(items) < effectivePageSize {
			break
		}
	}
	return models, nil
}

func (m Management) ListGateways(ctx context.Context, accountID string) ([]map[string]any, error) {
	return managementCall[[]map[string]any](ctx, m, accountID, http.MethodGet, "/ai-gateway/gateways", nil)
}

func (m Management) CreateGateway(ctx context.Context, accountID string, input map[string]any) (map[string]any, error) {
	// Cloudflare 的创建接口要求这些数字/布尔字段必填（缺省会报
	// "Expected number, received nan"），调用方只需给 id，其余用默认值补齐。
	payload := map[string]any{
		"cache_invalidate_on_update": false,
		"cache_ttl":                  0,
		"collect_logs":               true,
		"rate_limiting_interval":     0,
		"rate_limiting_limit":        0,
		"rate_limiting_technique":    "fixed",
	}
	for key, value := range input {
		payload[key] = value
	}
	return managementCall[map[string]any](ctx, m, accountID, http.MethodPost, "/ai-gateway/gateways", payload)
}

// AICapableAccounts lists enabled, reachable accounts whose token can use
// Workers AI. Management calls (model catalog, gateway CRUD) pick from these
// automatically so clients never have to choose an account themselves.
func (m Management) AICapableAccounts(ctx context.Context) ([]accounts.Account, error) {
	if m.Accounts == nil {
		return nil, errors.New("AI Gateway management is not configured")
	}
	items, err := m.Accounts.List(ctx)
	if err != nil {
		return nil, err
	}
	var capable []accounts.Account
	for _, account := range items {
		if account.Enabled && (account.HealthStatus == "healthy" || account.HealthStatus == "degraded") && hasAICapability(account) {
			capable = append(capable, account)
		}
	}
	return capable, nil
}

// PickAccount returns the account management calls should target when the
// caller did not specify one. Quota does not apply to management operations,
// so any AI-capable account works; invocation routing keeps its own
// quota-aware selection in Gateway.
func (m Management) PickAccount(ctx context.Context) (accounts.Account, error) {
	capable, err := m.AICapableAccounts(ctx)
	if err != nil {
		return accounts.Account{}, err
	}
	if len(capable) == 0 {
		return accounts.Account{}, ErrNoAICapableAccount
	}
	return capable[0], nil
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
	result, _, err := managementCallWithInfo[T](ctx, management, accountID, method, suffix, body)
	return result, err
}

type managementResultInfo struct {
	Page       int `json:"page"`
	PerPage    int `json:"per_page"`
	TotalPages int `json:"total_pages"`
}

func managementCallWithInfo[T any](ctx context.Context, management Management, accountID, method, suffix string, body any) (T, managementResultInfo, error) {
	var zero T
	var zeroInfo managementResultInfo
	if management.Accounts == nil {
		return zero, zeroInfo, errors.New("AI Gateway management is not configured")
	}
	account, err := management.Accounts.Get(ctx, accountID, true)
	if err != nil {
		return zero, zeroInfo, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return zero, zeroInfo, err
		}
		reader = bytes.NewReader(encoded)
	}
	baseURL := strings.TrimRight(management.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4"
	}
	request, err := http.NewRequestWithContext(ctx, method, baseURL+"/accounts/"+url.PathEscape(account.CloudflareAccountID)+suffix, reader)
	if err != nil {
		return zero, zeroInfo, err
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
		return zero, zeroInfo, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return zero, zeroInfo, err
	}
	var envelope struct {
		Success bool                 `json:"success"`
		Result  T                    `json:"result"`
		Info    managementResultInfo `json:"result_info"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return zero, zeroInfo, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.Success {
		message := fmt.Sprintf("Cloudflare AI Gateway returned HTTP %d", response.StatusCode)
		if len(envelope.Errors) > 0 && envelope.Errors[0].Message != "" {
			message = envelope.Errors[0].Message
		}
		return zero, zeroInfo, errors.New(message)
	}
	return envelope.Result, envelope.Info, nil
}

var ErrNoAICapableAccount = errors.New("no enabled account provides Workers AI")
