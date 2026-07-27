package webdavprotocol

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
)

func TestHandlerPutGetAndPropfind(t *testing.T) {
	t.Parallel()

	objects := &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}
	handler := Handler{
		Objects: objects,
		Verify: func(_ context.Context, username, password string) (Identity, error) {
			if username != "dav" || password != "secret" {
				return Identity{}, context.Canceled
			}
			return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
		},
	}

	put := httptest.NewRequest(http.MethodPut, "/docs/readme.txt", strings.NewReader("hello"))
	put.SetBasicAuth("dav", "secret")
	putResponse := httptest.NewRecorder()
	handler.ServeHTTP(putResponse, put)
	if putResponse.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d", putResponse.Code)
	}

	get := httptest.NewRequest(http.MethodGet, "/docs/readme.txt", nil)
	get.SetBasicAuth("dav", "secret")
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK || getResponse.Body.String() != "hello" {
		t.Fatalf("GET response = %d %q", getResponse.Code, getResponse.Body.String())
	}

	propfind := httptest.NewRequest("PROPFIND", "/docs/", nil)
	propfind.SetBasicAuth("dav", "secret")
	propfind.Header.Set("Depth", "1")
	propfindResponse := httptest.NewRecorder()
	handler.ServeHTTP(propfindResponse, propfind)
	if propfindResponse.Code != http.StatusMultiStatus || !strings.Contains(propfindResponse.Body.String(), "readme.txt") {
		t.Fatalf("PROPFIND response = %d %s", propfindResponse.Code, propfindResponse.Body.String())
	}
}

func TestHandlerPropfindEmptyRootCompatibility(t *testing.T) {
	t.Parallel()

	objects := &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}
	handler := Handler{
		Objects: objects,
		Verify: func(_ context.Context, username, password string) (Identity, error) {
			if username != "dav" || password != "secret" {
				return Identity{}, context.Canceled
			}
			return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
		},
	}

	assertResponses := func(response *httptest.ResponseRecorder, expected ...string) []propertyResponse {
		t.Helper()
		if response.Code != http.StatusMultiStatus {
			t.Fatalf("PROPFIND status = %d, body = %s", response.Code, response.Body.String())
		}
		var body multistatus
		if err := xml.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode PROPFIND response: %v", err)
		}
		if len(body.Responses) != len(expected) {
			t.Fatalf("PROPFIND responses = %#v, want hrefs %v", body.Responses, expected)
		}
		for index, href := range expected {
			entry := body.Responses[index]
			if entry.Href != href {
				t.Fatalf("PROPFIND response[%d] = %#v, want href %q", index, entry, href)
			}
		}
		return body.Responses
	}

	rootDepthOne := assertResponses(performPropfind(handler, "/", "1"), "/")
	if rootDepthOne[0].PropStat.Properties.ResourceType.Collection == nil || rootDepthOne[0].PropStat.Properties.DisplayName != "/" {
		t.Fatalf("root properties = %#v, want collection with display name /", rootDepthOne[0].PropStat.Properties)
	}

	rootDepthZero := assertResponses(performPropfind(handler, "/", "0"), "/")
	if rootDepthZero[0].PropStat.Properties.ResourceType.Collection == nil || rootDepthZero[0].PropStat.Properties.DisplayName != "/" {
		t.Fatalf("root properties = %#v, want collection with display name /", rootDepthZero[0].PropStat.Properties)
	}

	missingPlaceholder := performPropfind(handler, "/.empty/", "0")
	if missingPlaceholder.Code != http.StatusNotFound {
		t.Fatalf("PROPFIND synthetic collection status = %d, want %d", missingPlaceholder.Code, http.StatusNotFound)
	}

	if _, err := objects.Put(context.Background(), r2.PutRequest{
		Key: r2.WebDAVMountPrefix("credential") + "readme.txt", Body: strings.NewReader("hello"), Size: 5,
	}); err != nil {
		t.Fatalf("seed object: %v", err)
	}
	withRealObject := performPropfind(handler, "/", "1")
	entries := assertResponses(withRealObject, "/", "/readme.txt")
	if entries[0].PropStat.Properties.DisplayName != "/" ||
		entries[1].PropStat.Properties.DisplayName != "readme.txt" ||
		entries[1].PropStat.Properties.ResourceType.Collection != nil ||
		strings.Contains(withRealObject.Body.String(), "/.empty/") {
		t.Fatalf("PROPFIND response with real object = %s", withRealObject.Body.String())
	}
}

func TestHandlerCredentialNamespacesAreIsolated(t *testing.T) {
	t.Parallel()
	objects := &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}
	handler := Handler{
		Objects: objects,
		Verify: func(_ context.Context, username, password string) (Identity, error) {
			if password != "secret" || (username != "gamesync" && username != "test") {
				return Identity{}, context.Canceled
			}
			return Identity{ID: username + "-id", Scopes: []string{"r2:*"}}, nil
		},
	}
	put := func(username, body string) {
		request := httptest.NewRequest(http.MethodPut, "/same.txt", strings.NewReader(body))
		request.SetBasicAuth(username, "secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("%s PUT status = %d", username, response.Code)
		}
	}
	get := func(username string) string {
		request := httptest.NewRequest(http.MethodGet, "/same.txt", nil)
		request.SetBasicAuth(username, "secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s GET status = %d", username, response.Code)
		}
		return response.Body.String()
	}
	put("gamesync", "game")
	put("test", "test")
	if body := get("gamesync"); body != "game" {
		t.Fatalf("gamesync body = %q", body)
	}
	if body := get("test"); body != "test" {
		t.Fatalf("test body = %q", body)
	}
	for username, id := range map[string]string{"gamesync": "gamesync-id", "test": "test-id"} {
		response := performPropfindAs(handler, username, "/", "1")
		if response.Code != http.StatusMultiStatus || strings.Contains(response.Body.String(), r2.WebDAVNamespaceRoot) {
			t.Fatalf("%s PROPFIND = %d %s", username, response.Code, response.Body.String())
		}
		if _, ok := objects.values[r2.WebDAVMountPrefix(id)+"same.txt"]; !ok {
			t.Fatalf("missing scoped object for %s", username)
		}
	}
}

func performPropfind(handler Handler, target, depth string) *httptest.ResponseRecorder {
	return performPropfindAs(handler, "dav", target, depth)
}

func performPropfindAs(handler Handler, username, target, depth string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("PROPFIND", target, nil)
	request.SetBasicAuth(username, "secret")
	request.Header.Set("Depth", depth)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type memoryObjects struct {
	values   map[string][]byte
	metadata map[string]r2.Object
}

func (s *memoryObjects) Put(_ context.Context, request r2.PutRequest) (r2.Object, error) {
	data, _ := io.ReadAll(request.Body)
	object := r2.Object{Key: request.Key, Size: int64(len(data)), ETag: "etag", ContentType: request.ContentType, Metadata: request.Metadata, LastModified: time.Now()}
	s.values[request.Key], s.metadata[request.Key] = data, object
	return object, nil
}

func (s *memoryObjects) Get(_ context.Context, key string, _ r2.GetOptions) (r2.GetResult, error) {
	data, ok := s.values[key]
	if !ok {
		return r2.GetResult{}, r2.ErrObjectNotFound
	}
	object := s.metadata[key]
	return r2.GetResult{Body: io.NopCloser(bytes.NewReader(data)), Size: int64(len(data)), ETag: object.ETag, ContentType: object.ContentType}, nil
}

func (s *memoryObjects) Stat(_ context.Context, key string) (r2.Object, error) {
	object, ok := s.metadata[key]
	if !ok {
		return r2.Object{}, r2.ErrObjectNotFound
	}
	return object, nil
}

func (s *memoryObjects) List(_ context.Context, options r2.ListOptions) (r2.ObjectList, error) {
	result := r2.ObjectList{}
	for key, object := range s.metadata {
		if strings.HasPrefix(key, options.Prefix) {
			result.Objects = append(result.Objects, object)
		}
	}
	return result, nil
}

func (s *memoryObjects) Delete(_ context.Context, key string) error {
	if _, ok := s.values[key]; !ok {
		return r2.ErrObjectNotFound
	}
	delete(s.values, key)
	delete(s.metadata, key)
	return nil
}

func (s *memoryObjects) Copy(_ context.Context, source, destination string) (r2.Object, error) {
	data, ok := s.values[source]
	if !ok {
		return r2.Object{}, r2.ErrObjectNotFound
	}
	object := s.metadata[source]
	object.Key = destination
	s.values[destination] = append([]byte(nil), data...)
	s.metadata[destination] = object
	return object, nil
}
