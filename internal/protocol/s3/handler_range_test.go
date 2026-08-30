package s3protocol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
)

type unsatisfiedRangeObjects struct {
	*stubObjects
}

func (s *unsatisfiedRangeObjects) Get(context.Context, string, r2.GetOptions) (r2.GetResult, error) {
	return r2.GetResult{}, r2.ErrRangeNotSatisfiable
}

func TestGetMapsUnsatisfiedRangeAndReportsObjectLength(t *testing.T) {
	t.Parallel()
	const accessKey = "CFR2EXAMPLE"
	const secretKey = "super-secret-value"
	now := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	objects := &unsatisfiedRangeObjects{stubObjects: &stubObjects{
		values: map[string][]byte{"readme.txt": []byte("hello")},
		metadata: map[string]r2.Object{
			"readme.txt": {Key: "readme.txt", Size: 5, ETag: "etag"},
		},
	}}
	handler := testHandler(objects, accessKey, secretKey, now)
	request := signedRequest(t, http.MethodGet, "http://localhost/storage/readme.txt", nil, accessKey, secretKey, now)
	request.Header.Set("Range", "bytes=10-")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestedRangeNotSatisfiable || !strings.Contains(response.Body.String(), "InvalidRange") {
		t.Fatalf("GET response = %d %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Range"); got != "bytes */5" {
		t.Fatalf("Content-Range = %q, want %q", got, "bytes */5")
	}
}
