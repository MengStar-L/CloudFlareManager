package ai

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/ai/responsescompat"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/google/uuid"
)

type Gateway struct {
	Accounts         *accounts.Store
	DB               *sql.DB
	Policy           *ModelPolicy
	Estimator        *NeuronEstimator
	BaseURL          string
	HTTPClient       *http.Client
	NeuronSoftLimit  float64
	MaxRetryAccounts int
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
	EstimationSource string    `json:"neuron_estimation_source"`
	CreatedAt        time.Time `json:"created_at"`
}

func (g Gateway) Logs(ctx context.Context, limit int) ([]RequestLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := g.DB.QueryContext(ctx, `SELECT id, COALESCE(protocol_credential_id, ''), account_id, model,
		status_code, input_tokens, output_tokens, estimated_neurons, duration_ms, error_class,
		neuron_estimation_source, created_at
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
			&entry.InputTokens, &entry.OutputTokens, &entry.EstimatedNeurons, &entry.DurationMS, &entry.ErrorClass,
			&entry.EstimationSource, &created); err != nil {
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
	originalBody, err := io.ReadAll(http.MaxBytesReader(w, request.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read Workers AI request: %w", err)
	}
	body := originalBody
	model := modelFromRequest(request.URL.Path, originalBody)
	upstreamRequestPath := request.URL.Path
	var translated *responsescompat.TranslatedRequest
	if request.URL.Path == "/v1/responses" {
		prepared, translateErr := responsescompat.TranslateRequest(originalBody)
		if translateErr != nil {
			return translateErr
		}
		translated = &prepared
		body = prepared.ChatBody
		model = prepared.Model
		upstreamRequestPath = "/v1/chat/completions"
	}
	if g.Policy != nil {
		blocked, policyErr := g.Policy.IsBlocked(request.Context(), model)
		if policyErr != nil {
			return fmt.Errorf("check AI model policy: %w", policyErr)
		}
		if blocked {
			blockedErr := &ModelBlockedError{Model: model}
			if account, selectErr := g.selectAccount(request.Context(), nil); selectErr == nil {
				g.record(request.Context(), credentialID, account.ID, model, http.StatusForbidden,
					UsageMeasurement{Source: "paid_model_blocked"}, 0, blockedErr)
			}
			return blockedErr
		}
	}
	inputTokens := int64(len(originalBody) / 4)
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
		response, err := g.send(request.Context(), request, body, account, upstreamRequestPath)
		if err != nil {
			lastErr = err
			g.record(request.Context(), credentialID, account.ID, model, 0,
				g.measure(model, TokenUsage{Input: inputTokens}, false), time.Since(started), err)
			continue
		}
		if (response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500) && attempt+1 < maxAttempts {
			_ = response.Body.Close()
			lastErr = fmt.Errorf("Workers AI returned HTTP %d", response.StatusCode)
			g.record(request.Context(), credentialID, account.ID, model, response.StatusCode,
				g.measure(model, TokenUsage{Input: inputTokens}, false), time.Since(started), lastErr)
			continue
		}
		if response.StatusCode == http.StatusForbidden && g.Policy != nil {
			upstreamBody, readErr := readLimited(response.Body, 2<<20)
			_ = response.Body.Close()
			if readErr != nil {
				g.record(request.Context(), credentialID, account.ID, model, response.StatusCode,
					g.measure(model, TokenUsage{}, false), time.Since(started), readErr)
				return readErr
			}
			if reason, paid := PaidPlanReason(response.StatusCode, upstreamBody); paid {
				if learnErr := g.Policy.LearnPaid(request.Context(), model, reason); learnErr != nil {
					slog.Default().ErrorContext(request.Context(), "persist paid Workers AI model", "model", model, "error", learnErr)
				}
				copyResponseHeaders(w.Header(), response.Header)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(response.StatusCode)
				body := upstreamBody
				if translated != nil {
					body = responsescompat.NormalizeUpstreamError(response.StatusCode, upstreamBody)
				}
				_, copyErr := w.Write(body)
				blockedErr := &ModelBlockedError{Model: model}
				g.record(request.Context(), credentialID, account.ID, model, response.StatusCode,
					UsageMeasurement{Source: "paid_model_blocked"}, time.Since(started), blockedErr)
				return copyErr
			}
			response.Body = io.NopCloser(bytes.NewReader(upstreamBody))
		}
		if translated != nil {
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				upstreamBody, readErr := readLimited(response.Body, 2<<20)
				_ = response.Body.Close()
				if readErr != nil {
					g.record(request.Context(), credentialID, account.ID, model, response.StatusCode,
						g.measure(model, TokenUsage{}, false), time.Since(started), readErr)
					return readErr
				}
				copyResponseHeaders(w.Header(), response.Header)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(response.StatusCode)
				_, copyErr := w.Write(responsescompat.NormalizeUpstreamError(response.StatusCode, upstreamBody))
				g.record(request.Context(), credentialID, account.ID, model, response.StatusCode,
					g.measure(model, TokenUsage{}, false), time.Since(started), copyErr)
				return copyErr
			}
			copyResponseHeaders(w.Header(), response.Header)
			if translated.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(http.StatusOK)
				counter := &countingWriter{destination: w}
				capture := newUsageCaptureReader(response.Body)
				copyErr := responsescompat.TranslateStream(request.Context(), model, translated.OriginalBody, translated.ChatBody, capture, counter)
				_ = response.Body.Close()
				usage, exact := capture.Usage()
				if !exact {
					usage = TokenUsage{Input: inputTokens, Output: counter.bytes / 4}
				}
				g.record(request.Context(), credentialID, account.ID, model, http.StatusOK,
					g.measure(model, usage, true), time.Since(started), copyErr)
				return copyErr
			}
			upstreamBody, readErr := readLimited(response.Body, 16<<20)
			_ = response.Body.Close()
			if readErr != nil {
				g.record(request.Context(), credentialID, account.ID, model, http.StatusBadGateway,
					g.measure(model, TokenUsage{}, false), time.Since(started), readErr)
				return readErr
			}
			usage, exact := ExtractTokenUsage(upstreamBody)
			if !exact {
				usage = TokenUsage{Input: inputTokens}
			}
			translatedBody, translateErr := responsescompat.TranslateResponse(model, translated.OriginalBody, translated.ChatBody, upstreamBody)
			if translateErr != nil {
				if compatibilityErr, ok := translateErr.(*responsescompat.Error); ok {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(compatibilityErr.Status)
					_, _ = w.Write(responsescompat.ErrorResponse(compatibilityErr.Code, compatibilityErr.Message))
					g.record(request.Context(), credentialID, account.ID, model, compatibilityErr.Status,
						g.measure(model, usage, true), time.Since(started), translateErr)
					return nil
				}
				return translateErr
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, copyErr := w.Write(translatedBody)
			if !exact {
				usage.Output = int64(len(translatedBody)) / 4
			}
			g.record(request.Context(), credentialID, account.ID, model, http.StatusOK,
				g.measure(model, usage, true), time.Since(started), copyErr)
			return copyErr
		}
		defer response.Body.Close()
		copyResponseHeaders(w.Header(), response.Header)
		if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
			upstreamBody, readErr := readLimited(response.Body, 16<<20)
			if readErr != nil {
				g.record(request.Context(), credentialID, account.ID, model, http.StatusBadGateway,
					g.measure(model, TokenUsage{}, false), time.Since(started), readErr)
				return readErr
			}
			w.WriteHeader(response.StatusCode)
			_, copyErr := w.Write(upstreamBody)
			usage, exact := ExtractTokenUsage(upstreamBody)
			success := response.StatusCode >= 200 && response.StatusCode < 300
			if !exact {
				usage = TokenUsage{Input: inputTokens, Output: int64(len(upstreamBody)) / 4}
			}
			g.record(request.Context(), credentialID, account.ID, model, response.StatusCode,
				g.measure(model, usage, success), time.Since(started), copyErr)
			return copyErr
		}
		w.WriteHeader(response.StatusCode)
		counter := &countingWriter{destination: w}
		capture := newUsageCaptureReader(response.Body)
		copyErr := copyStreaming(counter, capture)
		usage, exact := capture.Usage()
		if !exact {
			usage = TokenUsage{Input: inputTokens, Output: counter.bytes / 4}
		}
		success := response.StatusCode >= 200 && response.StatusCode < 300
		g.record(request.Context(), credentialID, account.ID, model, response.StatusCode,
			g.measure(model, usage, success), time.Since(started), copyErr)
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

func (g Gateway) send(ctx context.Context, incoming *http.Request, body []byte, account accounts.Account, requestPath string) (*http.Response, error) {
	baseURL := strings.TrimRight(g.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4"
	}
	path := upstreamPath(account.CloudflareAccountID, requestPath)
	query := ""
	if incoming.URL.RawQuery != "" {
		query = "?" + incoming.URL.RawQuery
	}
	request, err := http.NewRequestWithContext(ctx, incoming.Method, baseURL+path+query, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+account.APIToken)
	contentType := incoming.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", incoming.Header.Get("Accept"))
	client := g.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 0}
	}
	return client.Do(request)
}

func readLimited(source io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(source, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("upstream response exceeds %d bytes", limit)
	}
	return body, nil
}

func (g Gateway) record(ctx context.Context, credentialID, accountID, model string, status int, measurement UsageMeasurement, duration time.Duration, requestErr error) {
	errorCount := 0
	errorClass := ""
	if requestErr != nil || status >= 400 {
		errorCount = 1
		var blocked *ModelBlockedError
		if errors.As(requestErr, &blocked) {
			errorClass = "paid_model_blocked"
		} else if requestErr != nil {
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
		measurement.Neurons, measurement.InputTokens, measurement.OutputTokens, errorCount, float64(duration.Microseconds())/1000)
	_, _ = g.DB.ExecContext(ctx, `INSERT INTO ai_request_logs(
		id, protocol_credential_id, account_id, model, status_code, input_tokens, output_tokens,
		estimated_neurons, duration_ms, error_class, created_at, neuron_estimation_source)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), nullableString(credentialID), accountID, model, status, measurement.InputTokens,
		measurement.OutputTokens, measurement.Neurons, float64(duration.Microseconds())/1000, errorClass,
		time.Now().UnixNano(), measurement.Source)
}

func (g Gateway) measure(model string, usage TokenUsage, success bool) UsageMeasurement {
	estimator := g.Estimator
	if estimator == nil {
		estimator = NewNeuronEstimator()
	}
	return estimator.Measure(model, usage, success)
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
