package ai

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/google/uuid"
)

type Gateway struct {
	Accounts         *accounts.Store
	DB               *sql.DB
	BaseURL          string
	HTTPClient       *http.Client
	NeuronSoftLimit  float64
	MaxRetryAccounts int
}

type Usage struct {
	AccountID        string  `json:"account_id"`
	Date             string  `json:"date"`
	EstimatedNeurons float64 `json:"estimated_neurons"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	LatencyMSTotal   float64 `json:"latency_ms_total"`
}

type RequestLog struct {
	ID               string    `json:"id"`
	CredentialID     string    `json:"credential_id,omitempty"`
	AccountID        string    `json:"account_id"`
	Model            string    `json:"model"`
	StatusCode       int       `json:"status_code"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	EstimatedNeurons float64   `json:"estimated_neurons"`
	DurationMS       float64   `json:"duration_ms"`
	ErrorClass       string    `json:"error_class,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

func (g Gateway) Usage(ctx context.Context, accountID, date string) ([]Usage, error) {
	query := `SELECT account_id, usage_date, estimated_neurons, input_tokens, output_tokens,
		requests, errors, latency_ms_total FROM ai_usage_daily WHERE 1 = 1`
	args := []any{}
	if accountID != "" {
		query += " AND account_id = ?"
		args = append(args, accountID)
	}
	if date != "" {
		query += " AND usage_date = ?"
		args = append(args, date)
	}
	query += " ORDER BY usage_date DESC, account_id"
	rows, err := g.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Usage
	for rows.Next() {
		var usage Usage
		if err := rows.Scan(&usage.AccountID, &usage.Date, &usage.EstimatedNeurons, &usage.InputTokens,
			&usage.OutputTokens, &usage.Requests, &usage.Errors, &usage.LatencyMSTotal); err != nil {
			return nil, err
		}
		result = append(result, usage)
	}
	return result, rows.Err()
}

func (g Gateway) Logs(ctx context.Context, limit int) ([]RequestLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := g.DB.QueryContext(ctx, `SELECT id, COALESCE(protocol_credential_id, ''), account_id, model,
		status_code, input_tokens, output_tokens, estimated_neurons, duration_ms, error_class, created_at
		FROM ai_request_logs ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RequestLog
	for rows.Next() {
		var entry RequestLog
		var created int64
		if err := rows.Scan(&entry.ID, &entry.CredentialID, &entry.AccountID, &entry.Model, &entry.StatusCode,
			&entry.InputTokens, &entry.OutputTokens, &entry.EstimatedNeurons, &entry.DurationMS, &entry.ErrorClass, &created); err != nil {
			return nil, err
		}
		entry.CreatedAt = time.Unix(0, created)
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (g Gateway) Forward(w http.ResponseWriter, request *http.Request, credentialID string) error {
	if g.Accounts == nil || g.DB == nil {
		return errors.New("Workers AI gateway is not configured")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, request.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read Workers AI request: %w", err)
	}
	model := modelFromRequest(request.URL.Path, body)
	inputTokens := int64(len(body) / 4)
	if inputTokens == 0 && len(body) > 0 {
		inputTokens = 1
	}
	maxAttempts := g.MaxRetryAccounts
	if maxAttempts <= 0 {
		maxAttempts = 2
	}
	excluded := make(map[string]bool)
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		account, err := g.selectAccount(request.Context(), excluded)
		if err != nil {
			if lastErr != nil {
				return lastErr
			}
			return err
		}
		excluded[account.ID] = true
		started := time.Now()
		response, err := g.send(request.Context(), request, body, account)
		if err != nil {
			lastErr = err
			g.record(request.Context(), credentialID, account.ID, model, 0, inputTokens, 0, time.Since(started), err)
			continue
		}
		if (response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500) && attempt+1 < maxAttempts {
			_ = response.Body.Close()
			lastErr = fmt.Errorf("Workers AI returned HTTP %d", response.StatusCode)
			g.record(request.Context(), credentialID, account.ID, model, response.StatusCode, inputTokens, 0, time.Since(started), lastErr)
			continue
		}
		defer response.Body.Close()
		copyResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		counter := &countingWriter{destination: w}
		copyErr := copyStreaming(counter, response.Body)
		outputTokens := counter.bytes / 4
		g.record(request.Context(), credentialID, account.ID, model, response.StatusCode, inputTokens, outputTokens, time.Since(started), copyErr)
		return copyErr
	}
	return lastErr
}

func (g Gateway) selectAccount(ctx context.Context, excluded map[string]bool) (accounts.Account, error) {
	items, err := g.Accounts.List(ctx)
	if err != nil {
		return accounts.Account{}, err
	}
	states := make([]AccountState, 0, len(items))
	for _, account := range items {
		if excluded[account.ID] || !account.Enabled || (account.HealthStatus != "healthy" && account.HealthStatus != "degraded") || !hasAICapability(account) {
			continue
		}
		state := AccountState{ID: account.ID, Healthy: true}
		var requests, errorsCount int64
		var latency float64
		err := g.DB.QueryRowContext(ctx, `SELECT estimated_neurons, requests, errors, latency_ms_total
			FROM ai_usage_daily WHERE account_id = ? AND usage_date = ?`, account.ID, time.Now().UTC().Format("2006-01-02")).Scan(
			&state.EstimatedNeurons, &requests, &errorsCount, &latency)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return accounts.Account{}, err
		}
		if requests > 0 {
			state.RecentErrorRatio = float64(errorsCount) / float64(requests)
			state.LatencyRatio = minFloat((latency/float64(requests))/10_000, 1)
		}
		states = append(states, state)
	}
	selected, err := (Router{NeuronSoftLimit: g.NeuronSoftLimit}).Select(states)
	if err != nil {
		return accounts.Account{}, err
	}
	return g.Accounts.Get(ctx, selected.ID, true)
}

func (g Gateway) send(ctx context.Context, incoming *http.Request, body []byte, account accounts.Account) (*http.Response, error) {
	baseURL := strings.TrimRight(g.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4"
	}
	path := upstreamPath(account.CloudflareAccountID, incoming.URL.Path)
	query := ""
	if incoming.URL.RawQuery != "" {
		query = "?" + incoming.URL.RawQuery
	}
	request, err := http.NewRequestWithContext(ctx, incoming.Method, baseURL+path+query, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+account.APIToken)
	request.Header.Set("Content-Type", incoming.Header.Get("Content-Type"))
	request.Header.Set("Accept", incoming.Header.Get("Accept"))
	client := g.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 0}
	}
	return client.Do(request)
}

func (g Gateway) record(ctx context.Context, credentialID, accountID, model string, status int, inputTokens, outputTokens int64, duration time.Duration, requestErr error) {
	neurons := EstimateNeurons(model, inputTokens, outputTokens)
	errorCount := 0
	errorClass := ""
	if requestErr != nil || status >= 400 {
		errorCount = 1
		if requestErr != nil {
			errorClass = "upstream_error"
		} else {
			errorClass = fmt.Sprintf("http_%d", status)
		}
	}
	_, _ = g.DB.ExecContext(ctx, `INSERT INTO ai_usage_daily(
		account_id, usage_date, estimated_neurons, input_tokens, output_tokens, requests, errors, latency_ms_total)
		VALUES(?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(account_id, usage_date) DO UPDATE SET
		estimated_neurons = estimated_neurons + excluded.estimated_neurons,
		input_tokens = input_tokens + excluded.input_tokens,
		output_tokens = output_tokens + excluded.output_tokens,
		requests = requests + 1, errors = errors + excluded.errors,
		latency_ms_total = latency_ms_total + excluded.latency_ms_total`, accountID, time.Now().UTC().Format("2006-01-02"),
		neurons, inputTokens, outputTokens, errorCount, float64(duration.Microseconds())/1000)
	_, _ = g.DB.ExecContext(ctx, `INSERT INTO ai_request_logs(
		id, protocol_credential_id, account_id, model, status_code, input_tokens, output_tokens,
		estimated_neurons, duration_ms, error_class, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), nullableString(credentialID), accountID, model, status, inputTokens, outputTokens, neurons,
		float64(duration.Microseconds())/1000, errorClass, time.Now().UnixNano())
}

func EstimateNeurons(model string, inputTokens, outputTokens int64) float64 {
	factor := 0.001
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "embedding") || strings.Contains(lower, "bge"):
		factor = 0.0002
	case strings.Contains(lower, "70b"):
		factor = 0.004
	case strings.Contains(lower, "vision"):
		factor = 0.003
	}
	return float64(inputTokens+outputTokens) * factor
}

func upstreamPath(accountID, incomingPath string) string {
	prefix := "/accounts/" + url.PathEscape(accountID) + "/ai"
	if strings.HasPrefix(incomingPath, "/v1/run/") {
		return prefix + "/run/" + strings.TrimPrefix(incomingPath, "/v1/run/")
	}
	return prefix + incomingPath
}

func modelFromRequest(path string, body []byte) string {
	if strings.HasPrefix(path, "/v1/run/") {
		return strings.TrimPrefix(path, "/v1/run/")
	}
	var envelope struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &envelope)
	return envelope.Model
}

func hasAICapability(account accounts.Account) bool {
	for _, capability := range account.Capabilities {
		if capability.Name == "ai" {
			return capability.Available
		}
	}
	return false
}

func copyResponseHeaders(destination, source http.Header) {
	for _, name := range []string{"Content-Type", "Cache-Control", "Retry-After", "X-Request-ID"} {
		if value := source.Get(name); value != "" {
			destination.Set(name, value)
		}
	}
	if strings.HasPrefix(source.Get("Content-Type"), "text/event-stream") {
		destination.Set("X-Accel-Buffering", "no")
	}
}

func copyStreaming(destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 32<<10)
	for {
		count, readErr := source.Read(buffer)
		if count > 0 {
			if _, err := destination.Write(buffer[:count]); err != nil {
				return err
			}
			if flusher, ok := destination.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

type countingWriter struct {
	destination io.Writer
	bytes       int64
}

func (w *countingWriter) Write(data []byte) (int, error) {
	count, err := w.destination.Write(data)
	w.bytes += int64(count)
	return count, err
}

func (w *countingWriter) Flush() {
	if flusher, ok := w.destination.(http.Flusher); ok {
		flusher.Flush()
	}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func minFloat(left, right float64) float64 {
	if left < right {
		return left
	}
	return right
}
