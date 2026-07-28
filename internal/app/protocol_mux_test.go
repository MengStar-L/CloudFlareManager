package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProtocolMuxRoutesWithoutChangingRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		method        string
		target        string
		authorization string
		want          string
	}{
		{name: "sigv4", method: http.MethodGet, target: "/storage/file", authorization: "AWS4-HMAC-SHA256 credential", want: "s3"},
		{name: "signed ai-looking path", method: http.MethodGet, target: "/v1/models", authorization: "AWS4-HMAC-SHA256 credential", want: "s3"},
		{name: "presigned", method: http.MethodGet, target: "/storage/file?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=value", want: "s3"},
		{name: "basic", method: http.MethodGet, target: "/document.txt", authorization: "Basic ZGF2OnNlY3JldA==", want: "webdav"},
		{name: "basic ai-looking path", method: http.MethodGet, target: "/v1/models", authorization: "Basic ZGF2OnNlY3JldA==", want: "webdav"},
		{name: "propfind discovery", method: "PROPFIND", target: "/", want: "webdav"},
		{name: "mkcol", method: "MKCOL", target: "/folder", want: "webdav"},
		{name: "copy", method: "COPY", target: "/source", want: "webdav"},
		{name: "move", method: "MOVE", target: "/source", want: "webdav"},
		{name: "lock", method: "LOCK", target: "/file", want: "webdav"},
		{name: "unlock", method: "UNLOCK", target: "/file", want: "webdav"},
		{name: "options", method: http.MethodOptions, target: "/", want: "webdav"},
		{name: "models", method: http.MethodGet, target: "/v1/models", want: "ai"},
		{name: "chat", method: http.MethodPost, target: "/v1/chat/completions", authorization: "Bearer token", want: "ai"},
		{name: "responses", method: http.MethodPost, target: "/v1/responses", want: "ai"},
		{name: "embeddings", method: http.MethodPost, target: "/v1/embeddings", want: "ai"},
		{name: "native model", method: http.MethodPost, target: "/v1/run/@cf/meta/model", want: "ai"},
		{name: "unsupported v1 path", method: http.MethodGet, target: "/v1/unknown", want: "admin"},
		{name: "admin api", method: http.MethodGet, target: "/api/v1/session", want: "admin"},
		{name: "panel", method: http.MethodGet, target: "/", want: "admin"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mux := protocolMux{
				Admin: markerHandler("admin"), S3: markerHandler("s3"),
				WebDAV: markerHandler("webdav"), AI: markerHandler("ai"),
			}
			request := httptest.NewRequest(test.method, "http://manager.example.com"+test.target, nil)
			request.Header.Set("Authorization", test.authorization)
			originalPath, originalHost := request.URL.RequestURI(), request.Host
			response := httptest.NewRecorder()

			mux.ServeHTTP(response, request)

			if got := response.Header().Get("X-Test-Handler"); got != test.want {
				t.Fatalf("handler = %q, want %q", got, test.want)
			}
			if request.URL.RequestURI() != originalPath || request.Host != originalHost {
				t.Fatalf("request changed to host=%q path=%q", request.Host, request.URL.RequestURI())
			}
		})
	}
}

func markerHandler(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test-Handler", name)
		w.WriteHeader(http.StatusNoContent)
	})
}
