package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifierDetectsCapabilities(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/graphql" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"viewer": map[string]any{}}})
			return
		}
		switch r.URL.Path {
		case "/user/tokens/verify", "/accounts/account/r2/buckets", "/accounts/account/d1/database", "/accounts/account/ai/models/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	verifier := Verifier{BaseURL: server.URL, Client: server.Client()}
	capabilities, err := verifier.Detect(context.Background(), "account", "token")
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 5 {
		t.Fatalf("capability count = %d", len(capabilities))
	}
	for _, capability := range capabilities {
		if !capability.Available {
			t.Fatalf("capability unavailable: %#v", capability)
		}
	}
}

func TestVerifierFailureDiagnostics(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		capability string
		status     int
		body       string
		want       []string
		wantErr    bool
	}{
		{name: "unauthorized with API code", capability: "api_token", status: 401, body: `{"success":false,"errors":[{"code":9109,"message":"Invalid access token private-token"}]}`, want: []string{"API Token 验证失败", "HTTP 401", "9109", "Invalid access token [redacted]", "过期或被撤销"}},
		{name: "forbidden R2", capability: "r2", status: 403, body: `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`, want: []string{"R2 桶列表读取失败", "HTTP 403", "10000", "Workers R2 Storage Read", "未验证 R2 Access Key"}},
		{name: "plain unauthorized", capability: "api_token", status: 401, body: "Unauthorized", want: []string{"HTTP 401", "身份验证未通过"}},
		{name: "proxy HTML", capability: "d1", status: 502, body: "<html>private-token</html>", want: []string{"HTTP 502", "代理服务异常", "Bad Gateway"}},
		{name: "rate limit", capability: "ai", status: 429, body: "Too Many Requests", want: []string{"HTTP 429", "请求频率受限"}},
		{name: "missing account", capability: "r2", status: 404, body: `{"success":false,"errors":[{"code":7003,"message":"Could not route"}]}`, want: []string{"HTTP 404", "Cloudflare Account ID", "7003"}},
		{name: "GraphQL errors on HTTP 200", capability: "analytics", status: 200, body: `{"data":null,"errors":[{"message":"not authorized for account private-token"}]}`, want: []string{"账号分析 GraphQL 查询失败", "HTTP 200", "not authorized for account [redacted]"}},
		{name: "inactive token", capability: "api_token", status: 200, body: `{"success":true,"result":{"status":"expired"}}`, want: []string{"API Token 验证失败", "Token status: expired"}},
		{name: "malformed success", capability: "d1", status: 200, body: "<html>private-token</html>", want: []string{"D1 数据库列表读取", "HTTP 200", "检测结果无法确认"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			available, detail, err := runProbe(context.Background(), server.Client(), server.URL, "private-token", test.capability, http.MethodGet, "/probe", nil)
			if available || (err != nil) != test.wantErr {
				t.Fatalf("available = %v, detail = %q, error = %v", available, detail, err)
			}
			if err != nil {
				detail = err.Error()
			}
			for _, want := range append(test.want, "GET /probe") {
				if !strings.Contains(detail, want) {
					t.Errorf("diagnostic %q missing %q", detail, want)
				}
			}
			if strings.Contains(detail, "private-token") || strings.Contains(detail, "<html>") {
				t.Errorf("diagnostic leaked token or raw HTML: %q", detail)
			}
		})
	}
}

func TestVerifierPreservesBothTokenVerificationFailures(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/tokens/verify":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":9109,"message":"User token rejected"}]}`)
		case "/accounts/account/tokens/verify":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":10000,"message":"Account token rejected"}]}`)
		default:
			_, _ = io.WriteString(w, `{"success":true,"result":[]}`)
		}
	}))
	defer server.Close()
	capabilities, err := (Verifier{BaseURL: server.URL, Client: server.Client()}).Detect(context.Background(), "account", "private-token")
	if err != nil {
		t.Fatal(err)
	}
	if capabilities[0].Available {
		t.Fatal("failed token checks marked available")
	}
	for _, want := range []string{"/user/tokens/verify", "/accounts/account/tokens/verify", "HTTP 401", "HTTP 403", "9109", "10000", "User token rejected", "Account token rejected"} {
		if !strings.Contains(capabilities[0].Detail, want) {
			t.Errorf("token diagnostics missing %q: %s", want, capabilities[0].Detail)
		}
	}
}

func TestVerifierNetworkErrorIdentifiesProbeAndRedactsToken(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: failingProbeTransport{}}
	_, err := (Verifier{BaseURL: "https://cloudflare.invalid", Client: client}).Detect(context.Background(), "account", "private-token")
	if err == nil {
		t.Fatal("expected network error")
	}
	for _, want := range []string{"API Token 验证", "GET /user/tokens/verify", "尚不能判断凭证是否有效", "[redacted]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("network error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "private-token") {
		t.Fatalf("network error leaked API token: %v", err)
	}
}

type failingProbeTransport struct{}

func (failingProbeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection failed for private-token")
}

// Account-owned API tokens are rejected by /user/tokens/verify; the verifier
// must fall back to the account-scoped verify endpoint.
func TestVerifierAcceptsAccountOwnedToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/tokens/verify":
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		case "/accounts/account/tokens/verify":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"status": "active"}})
		case "/graphql":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"viewer": map[string]any{}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []any{}})
		}
	}))
	defer server.Close()

	verifier := Verifier{BaseURL: server.URL, Client: server.Client()}
	capabilities, err := verifier.Detect(context.Background(), "account", "token")
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range capabilities {
		if capability.Name == "api_token" && !capability.Available {
			t.Fatalf("api_token should be available via account-scoped verify: %#v", capability)
		}
	}
}
