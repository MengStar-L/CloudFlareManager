package aiprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	aimodule "github.com/cf-r2-manager/cf-r2-manager/internal/modules/ai"
)

type Identity struct {
	ID     string
	Scopes []string
}

func (i Identity) hasScope(scope string) bool {
	for _, candidate := range i.Scopes {
		if candidate == scope || candidate == "ai:*" || candidate == "*" {
			return true
		}
	}
	return false
}

type Verifier func(context.Context, string, string) (Identity, error)

type Handler struct {
	Gateway *aimodule.Gateway
	Verify  Verifier
}

func (h Handler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" && request.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if !supportedRoute(request.Method, request.URL.Path) {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "The requested AI endpoint does not exist")
		return
	}
	publicID, secret, ok := parseBearer(request.Header.Get("Authorization"))
	if !ok || h.Verify == nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "A valid AI API key is required")
		return
	}
	identity, err := h.Verify(request.Context(), publicID, secret)
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "The AI API key is invalid or revoked")
		return
	}
	if !identity.hasScope("ai:invoke") {
		writeOpenAIError(w, http.StatusForbidden, "insufficient_scope", "The AI API key does not allow model invocation")
		return
	}
	if h.Gateway == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "gateway_unavailable", "Workers AI gateway is unavailable")
		return
	}
	tracked := &responseTracker{ResponseWriter: w}
	if err := h.Gateway.Forward(tracked, request, identity.ID); err != nil && !tracked.wroteHeader {
		status := http.StatusBadGateway
		code := "upstream_error"
		if errors.Is(err, aimodule.ErrAIQuotaExceeded) {
			status, code = http.StatusTooManyRequests, "ai_quota_exceeded"
		}
		writeOpenAIError(w, status, code, err.Error())
	}
}

func supportedRoute(method, path string) bool {
	if method == http.MethodGet && path == "/v1/models" {
		return true
	}
	if method != http.MethodPost {
		return false
	}
	if path == "/v1/chat/completions" || path == "/v1/responses" || path == "/v1/embeddings" {
		return true
	}
	return strings.HasPrefix(path, "/v1/run/") && len(strings.TrimPrefix(path, "/v1/run/")) > 0
}

func parseBearer(value string) (string, string, bool) {
	if !strings.HasPrefix(value, "Bearer ") {
		return "", "", false
	}
	publicID, secret, found := strings.Cut(strings.TrimSpace(strings.TrimPrefix(value, "Bearer ")), ".")
	return publicID, secret, found && publicID != "" && secret != ""
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": code, "code": code, "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type responseTracker struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *responseTracker) WriteHeader(status int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseTracker) Write(data []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(data)
}

func (w *responseTracker) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
