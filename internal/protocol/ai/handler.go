package aiprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	aimodule "github.com/cf-r2-manager/cf-r2-manager/internal/modules/ai"
	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/ai/responsescompat"
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
type ModelCatalog func(context.Context) ([]map[string]any, error)

type Handler struct {
	Gateway *aimodule.Gateway
	Verify  Verifier
	Models  ModelCatalog
}

func (h Handler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" && request.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if !Supports(request.Method, request.URL.Path) {
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
	if request.Method == http.MethodGet && request.URL.Path == "/v1/models" {
		h.serveModels(w, request)
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
		param := ""
		if errors.Is(err, aimodule.ErrAIQuotaExceeded) {
			status, code = http.StatusTooManyRequests, "ai_quota_exceeded"
		} else {
			var blockedErr *aimodule.ModelBlockedError
			var compatibilityErr *responsescompat.Error
			if errors.As(err, &blockedErr) {
				status, code = http.StatusForbidden, "model_not_available"
				err = errors.New(blockedErr.Error())
			} else if errors.As(err, &compatibilityErr) {
				status, code, param = compatibilityErr.Status, compatibilityErr.Code, compatibilityErr.Param
				err = errors.New(compatibilityErr.Message)
			}
		}
		writeOpenAIErrorParam(w, status, code, err.Error(), param)
	}
}

func (h Handler) serveModels(w http.ResponseWriter, request *http.Request) {
	if h.Models == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "model_catalog_unavailable", "Workers AI model catalog is unavailable")
		return
	}
	items, err := h.Models(request.Context())
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, aimodule.ErrNoAICapableAccount) {
			status = http.StatusServiceUnavailable
		}
		writeOpenAIError(w, status, "model_catalog_unavailable", err.Error())
		return
	}
	ids := make(map[string]struct{}, len(items))
	for _, item := range items {
		id, _ := item["name"].(string)
		if strings.TrimSpace(id) == "" {
			id, _ = item["id"].(string)
		}
		if id = strings.TrimSpace(id); id != "" {
			ids[id] = struct{}{}
		}
	}
	names := make([]string, 0, len(ids))
	for id := range ids {
		names = append(names, id)
	}
	sort.Strings(names)
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	models := make([]model, 0, len(names))
	for _, id := range names {
		models = append(models, model{ID: id, Object: "model", OwnedBy: "cloudflare"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func Supports(method, path string) bool {
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
	writeOpenAIErrorParam(w, status, code, message, "")
}

func writeOpenAIErrorParam(w http.ResponseWriter, status int, code, message, param string) {
	errorBody := map[string]string{"type": code, "code": code, "message": message}
	if param != "" {
		errorBody["param"] = param
	}
	writeJSON(w, status, map[string]any{"error": errorBody})
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
