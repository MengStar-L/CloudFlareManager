package app

import (
	"net/http"
	"strings"

	aiprotocol "github.com/cf-r2-manager/cf-r2-manager/internal/protocol/ai"
)

type protocolMux struct {
	Admin  http.Handler
	S3     http.Handler
	WebDAV http.Handler
	AI     http.Handler
}

func (m protocolMux) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	authorization := request.Header.Get("Authorization")
	switch {
	case hasAuthorizationScheme(authorization, "AWS4-HMAC-SHA256") || request.URL.Query().Get("X-Amz-Algorithm") == "AWS4-HMAC-SHA256":
		m.S3.ServeHTTP(w, request)
	case hasAuthorizationScheme(authorization, "Basic") || isWebDAVMethod(request.Method):
		m.WebDAV.ServeHTTP(w, request)
	case aiprotocol.Supports(request.Method, request.URL.Path):
		m.AI.ServeHTTP(w, request)
	default:
		m.Admin.ServeHTTP(w, request)
	}
}

func hasAuthorizationScheme(value, scheme string) bool {
	name, _, found := strings.Cut(strings.TrimSpace(value), " ")
	return found && strings.EqualFold(name, scheme)
}

func isWebDAVMethod(method string) bool {
	switch method {
	case "PROPFIND", "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK", http.MethodOptions:
		return true
	default:
		return false
	}
}
