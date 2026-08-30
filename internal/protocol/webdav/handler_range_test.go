package webdavprotocol

import (
	"context"
	"net/http"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
)

type unsatisfiedRangeObjects struct {
	*memoryObjects
}

func (s *unsatisfiedRangeObjects) Get(context.Context, string, r2.GetOptions) (r2.GetResult, error) {
	return r2.GetResult{}, r2.ErrRangeNotSatisfiable
}

func TestGetMapsUnsatisfiedRangeAndReportsObjectLength(t *testing.T) {
	t.Parallel()
	prefix := r2.WebDAVMountPrefix("credential")
	objects := &unsatisfiedRangeObjects{memoryObjects: &memoryObjects{
		values: map[string][]byte{prefix + "readme.txt": []byte("hello")},
		metadata: map[string]r2.Object{
			prefix + "readme.txt": {Key: prefix + "readme.txt", Size: 5, ETag: "etag"},
		},
	}}
	handler := Handler{
		Objects: objects,
		Verify: func(context.Context, string, string) (Identity, error) {
			return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
		},
	}

	response := performDAVRequest(handler, http.MethodGet, "/readme.txt", "", map[string]string{"Range": "bytes=10-"})
	if response.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("GET status = %d, want %d", response.Code, http.StatusRequestedRangeNotSatisfiable)
	}
	if got := response.Header().Get("Content-Range"); got != "bytes */5" {
		t.Fatalf("Content-Range = %q, want %q", got, "bytes */5")
	}
}
