package ai

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestGatewayBlocksLearnedPaidModelBeforeUpstream(t *testing.T) {
	t.Parallel()
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls.Add(1)
	}))
	defer upstream.Close()

	gateway, db, policy := newPolicyGateway(t, upstream)
	if err := policy.LearnPaid(context.Background(), "@cf/vendor/paid", "requires a Workers Paid plan"); err != nil {
		t.Fatal(err)
	}
	request := aiRequest(t, "@cf/vendor/paid")
	response := httptest.NewRecorder()
	err := gateway.Forward(response, request, "credential")
	var blocked *ModelBlockedError
	if !errors.As(err, &blocked) || blocked.Model != "@cf/vendor/paid" {
		t.Fatalf("error = %v", err)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("upstream calls = %d", upstreamCalls.Load())
	}
	var status int
	var neurons float64
	var errorClass string
	if err := db.QueryRow(`SELECT status_code, estimated_neurons, error_class FROM ai_request_logs
		WHERE model = ?`, "@cf/vendor/paid").Scan(&status, &neurons, &errorClass); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusForbidden || neurons != 0 || errorClass != "paid_model_blocked" {
		t.Fatalf("log = status %d, neurons %f, error %q", status, neurons, errorClass)
	}
}

func TestGatewayLearnsPaidPlanResponse(t *testing.T) {
	t.Parallel()
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"upstream_error","message":"AiError: Model @cf/vendor/new-paid is not available on the Workers Free plan: This model requires a Workers Paid plan."}}`))
	}))
	defer upstream.Close()

	gateway, db, _ := newPolicyGateway(t, upstream)
	first := httptest.NewRecorder()
	if err := gateway.Forward(first, aiRequest(t, "@cf/vendor/new-paid"), "credential"); err != nil {
		t.Fatal(err)
	}
	if first.Code != http.StatusForbidden || !strings.Contains(first.Body.String(), "Workers Paid plan") {
		t.Fatalf("first response = %d %s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	err := gateway.Forward(second, aiRequest(t, "@cf/vendor/new-paid"), "credential")
	var blocked *ModelBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("second error = %v", err)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d", upstreamCalls.Load())
	}
	var reason string
	if err := db.QueryRow("SELECT reason FROM ai_paid_models WHERE model_id = ?", "@cf/vendor/new-paid").Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "Workers Paid plan") || strings.Contains(reason, `{"error"`) {
		t.Fatalf("reason = %q", reason)
	}
}

func TestGatewayDoesNotLearnUnrelatedForbiddenResponse(t *testing.T) {
	t.Parallel()
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid API token"}}`))
	}))
	defer upstream.Close()

	gateway, db, _ := newPolicyGateway(t, upstream)
	for index := 0; index < 2; index++ {
		response := httptest.NewRecorder()
		if err := gateway.Forward(response, aiRequest(t, "@cf/vendor/free"), "credential"); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusForbidden {
			t.Fatalf("response = %d %s", response.Code, response.Body.String())
		}
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("upstream calls = %d", upstreamCalls.Load())
	}
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM ai_paid_models WHERE model_id = ?", "@cf/vendor/free").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("learned rows = %d", rows)
	}
}

func TestGatewayRecordsActualTokenUsageWithOfficialRate(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 50},
		})
	}))
	defer upstream.Close()

	gateway, db, _ := newPolicyGateway(t, upstream)
	gateway.Estimator = NewNeuronEstimator()
	response := httptest.NewRecorder()
	if err := gateway.Forward(response, aiRequest(t, "@cf/meta/llama-3.2-1b-instruct"), "credential"); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	var inputTokens, outputTokens int64
	var neurons float64
	var source string
	if err := db.QueryRow(`SELECT input_tokens, output_tokens, estimated_neurons, neuron_estimation_source
		FROM ai_request_logs WHERE model = ?`, "@cf/meta/llama-3.2-1b-instruct").Scan(
		&inputTokens, &outputTokens, &neurons, &source); err != nil {
		t.Fatal(err)
	}
	if inputTokens != 100 || outputTokens != 50 || math.Abs(neurons-1.1583) > 0.000001 || source != "official_rate_actual_tokens" {
		t.Fatalf("usage = %d/%d, neurons = %f, source = %q", inputTokens, outputTokens, neurons, source)
	}
}

func newPolicyGateway(t *testing.T, upstream *httptest.Server) (Gateway, *sql.DB, *ModelPolicy) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{18}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := accountStore.SetCapabilities(context.Background(), account.ID, []accounts.Capability{{Name: "ai", Available: true, CheckedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err := accountStore.SetHealth(context.Background(), account.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}
	policy := NewModelPolicy(db)
	return Gateway{
		Accounts: accountStore, DB: db, Policy: policy, BaseURL: upstream.URL,
		HTTPClient: upstream.Client(), NeuronSoftLimit: 9_000,
	}, db, policy
}

func aiRequest(t *testing.T, model string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": model, "messages": []any{map[string]string{"role": "user", "content": "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}
