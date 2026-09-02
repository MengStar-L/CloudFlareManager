package webdavprotocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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

func TestHandlerPropfindReadsMoreThanOneIndexPage(t *testing.T) {
	t.Parallel()
	objects := &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}
	prefix := r2.WebDAVMountPrefix("credential")
	for index := 0; index < 1001; index++ {
		key := prefix + fmt.Sprintf("file-%04d.txt", index)
		objects.metadata[key] = r2.Object{Key: key, Size: 1, ETag: key, LastModified: time.Now()}
	}
	handler := Handler{Objects: objects, Verify: func(context.Context, string, string) (Identity, error) {
		return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
	}}
	response := performPropfind(handler, "/", "1")
	if response.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d", response.Code)
	}
	var body multistatus
	if err := xml.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Responses) != 1002 {
		t.Fatalf("responses = %d, want root plus 1001 children", len(body.Responses))
	}
}

func TestHandlerPropfindRepairsOnlyListedObjectsWithoutStrongETags(t *testing.T) {
	t.Parallel()
	base := &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}
	prefix := r2.WebDAVMountPrefix("credential")
	base.metadata[prefix+"repaired.txt"] = r2.Object{Key: prefix + "repaired.txt", Size: 1, ETag: "repaired", LastModified: time.Now()}
	base.metadata[prefix+"valid.txt"] = r2.Object{Key: prefix + "valid.txt", Size: 1, ETag: "valid", LastModified: time.Now()}
	objects := &staleListETagObjects{memoryObjects: base, staleKey: prefix + "repaired.txt"}
	handler := Handler{Objects: objects, Verify: func(context.Context, string, string) (Identity, error) {
		return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
	}}

	response := performPropfind(handler, "/", "1")
	if response.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body multistatus
	if err := xml.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	etags := make(map[string]string, len(body.Responses))
	for _, entry := range body.Responses {
		etags[entry.Href] = entry.PropStat.Properties.ETag
	}
	if etags["/repaired.txt"] != `"repaired"` || etags["/valid.txt"] != `"valid"` {
		t.Fatalf("PROPFIND ETags = %#v", etags)
	}
	wantStatKeys := prefix + "," + prefix + "repaired.txt"
	if got := strings.Join(objects.statKeys, ","); got != wantStatKeys {
		t.Fatalf("Stat keys = %q, want root validation plus only the invalid listed object %q", got, wantStatKeys)
	}
}

func TestWriteObjectStatusMapsQuotaAndWriteConflict(t *testing.T) {
	for _, test := range []struct {
		err  error
		want int
	}{
		{r2.ErrQuotaExceeded, http.StatusInsufficientStorage},
		{r2.ErrWriteInProgress, http.StatusLocked},
		{r2.ErrBucketDeleting, http.StatusServiceUnavailable},
	} {
		response := httptest.NewRecorder()
		writeObjectStatus(response, test.err)
		if response.Code != test.want {
			t.Fatalf("error %v status = %d, want %d", test.err, response.Code, test.want)
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

type staleListETagObjects struct {
	*memoryObjects
	staleKey string
	statKeys []string
}

func (s *staleListETagObjects) Stat(ctx context.Context, key string) (r2.Object, error) {
	s.statKeys = append(s.statKeys, key)
	return s.memoryObjects.Stat(ctx, key)
}

func (s *staleListETagObjects) List(ctx context.Context, options r2.ListOptions) (r2.ObjectList, error) {
	result, err := s.memoryObjects.List(ctx, options)
	if err != nil {
		return result, err
	}
	for index := range result.Objects {
		if result.Objects[index].Key == s.staleKey {
			result.Objects[index].ETag = ""
		}
	}
	return result, nil
}

func (s *memoryObjects) Put(_ context.Context, request r2.PutRequest) (r2.Object, error) {
	result, err := s.PutConditional(context.Background(), request)
	return result.Object, err
}

func (s *memoryObjects) PutConditional(_ context.Context, request r2.PutRequest) (r2.PutResult, error) {
	data, _ := io.ReadAll(request.Body)
	existing, exists := s.metadata[request.Key]
	if !memoryConditionsMatch(existing, exists, request.Conditions) {
		return r2.PutResult{}, r2.ErrPreconditionFailed
	}
	digest := sha256.Sum256(data)
	object := r2.Object{Key: request.Key, Size: int64(len(data)), ETag: fmt.Sprintf("%x", digest), ContentType: request.ContentType, Metadata: request.Metadata, LastModified: time.Now()}
	s.values[request.Key], s.metadata[request.Key] = data, object
	return r2.PutResult{Object: object, Created: !exists}, nil
}

func (s *memoryObjects) Get(_ context.Context, key string, options r2.GetOptions) (r2.GetResult, error) {
	data, ok := s.values[key]
	if !ok {
		return r2.GetResult{}, r2.ErrObjectNotFound
	}
	object := s.metadata[key]
	if options.IfMatch != "" && strings.Trim(options.IfMatch, `"`) != object.ETag {
		return r2.GetResult{}, r2.ErrConditionalRequestConflict
	}
	return r2.GetResult{Body: io.NopCloser(bytes.NewReader(data)), Size: int64(len(data)), ETag: object.ETag, ContentType: object.ContentType, LastModified: object.LastModified}, nil
}

func (s *memoryObjects) Stat(_ context.Context, key string) (r2.Object, error) {
	object, ok := s.metadata[key]
	if !ok {
		return r2.Object{}, r2.ErrObjectNotFound
	}
	return object, nil
}

func (s *memoryObjects) List(_ context.Context, options r2.ListOptions) (r2.ObjectList, error) {
	var keys []string
	for key, object := range s.metadata {
		_ = object
		if strings.HasPrefix(key, options.Prefix) && key > options.After {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	limit := options.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	result := r2.ObjectList{}
	for index, key := range keys {
		if index == limit {
			result.NextMarker = keys[index-1]
			break
		}
		result.Objects = append(result.Objects, s.metadata[key])
	}
	return result, nil
}

func (s *memoryObjects) Delete(_ context.Context, key string) error {
	return s.DeleteConditional(context.Background(), key, r2.MutationConditions{})
}

func (s *memoryObjects) DeleteConditional(_ context.Context, key string, conditions r2.MutationConditions) error {
	if _, ok := s.values[key]; !ok {
		return r2.ErrObjectNotFound
	}
	if !memoryConditionsMatch(s.metadata[key], true, conditions) {
		return r2.ErrPreconditionFailed
	}
	delete(s.values, key)
	delete(s.metadata, key)
	return nil
}

func (s *memoryObjects) Copy(_ context.Context, source, destination string) (r2.Object, error) {
	result, err := s.CopyConditional(context.Background(), source, destination, r2.MutationConditions{})
	return result.Object, err
}

func (s *memoryObjects) CopyConditional(_ context.Context, source, destination string, conditions r2.MutationConditions) (r2.PutResult, error) {
	data, ok := s.values[source]
	if !ok {
		return r2.PutResult{}, r2.ErrObjectNotFound
	}
	existing, exists := s.metadata[destination]
	if !memoryConditionsMatch(existing, exists, conditions) {
		return r2.PutResult{}, r2.ErrPreconditionFailed
	}
	object := s.metadata[source]
	object.Key = destination
	object.LastModified = time.Now()
	s.values[destination] = append([]byte(nil), data...)
	s.metadata[destination] = object
	return r2.PutResult{Object: object, Created: !exists}, nil
}

func memoryConditionsMatch(object r2.Object, exists bool, conditions r2.MutationConditions) bool {
	if conditions.IfMatch != nil {
		matched := conditions.IfMatch.Wildcard && exists
		for _, tag := range conditions.IfMatch.Tags {
			matched = matched || exists && !tag.Weak && tag.Value == object.ETag
		}
		if !matched {
			return false
		}
	} else if conditions.IfUnmodifiedSince != nil && exists && object.LastModified.Truncate(time.Second).After(conditions.IfUnmodifiedSince.UTC().Truncate(time.Second)) {
		return false
	}
	if conditions.IfNoneMatch != nil && exists {
		if conditions.IfNoneMatch.Wildcard {
			return false
		}
		for _, tag := range conditions.IfNoneMatch.Tags {
			if tag.Value == object.ETag {
				return false
			}
		}
	}
	return true
}
