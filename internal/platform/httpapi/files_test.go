package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/audit"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/auth"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestFilesAPIEmptyUploadPreviewAndDirectoryJob(t *testing.T) {
	t.Parallel()
	fixture := newFilesAPIFixture(t)

	empty := fixture.request(t, http.MethodGet, "/api/v1/files", nil, "")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"directory_count":0`) {
		t.Fatalf("empty listing = %d %s", empty.Code, empty.Body.String())
	}

	created := fixture.request(t, http.MethodPost, "/api/v1/files/directories", map[string]string{"path": "docs/"}, "application/json")
	if created.Code != http.StatusCreated {
		t.Fatalf("create directory = %d %s", created.Code, created.Body.String())
	}
	uploaded := fixture.request(t, http.MethodPut, "/api/v1/files/content?key=docs%2Freadme.txt", []byte("hello"), "text/plain")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload = %d %s", uploaded.Code, uploaded.Body.String())
	}
	conflict := fixture.request(t, http.MethodPut, "/api/v1/files/content?key=docs%2Freadme.txt", []byte("again"), "text/plain")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("upload conflict = %d %s", conflict.Code, conflict.Body.String())
	}

	listing := fixture.request(t, http.MethodGet, "/api/v1/files?path=docs%2F", nil, "")
	if listing.Code != http.StatusOK || !strings.Contains(listing.Body.String(), `"key":"docs/readme.txt"`) {
		t.Fatalf("directory listing = %d %s", listing.Code, listing.Body.String())
	}
	preview := fixture.request(t, http.MethodGet, "/api/v1/files/content?key=docs%2Freadme.txt&mode=preview", nil, "")
	if preview.Code != http.StatusOK || preview.Body.String() != "hello" {
		t.Fatalf("preview = %d %q", preview.Code, preview.Body.String())
	}
	if got := preview.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("preview content type = %q", got)
	}
	download := fixture.request(t, http.MethodGet, "/api/v1/files/content?key=docs%2Freadme.txt&mode=download", nil, "")
	if download.Code != http.StatusOK || !strings.HasPrefix(download.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("download = %d, disposition %q", download.Code, download.Header().Get("Content-Disposition"))
	}

	operation := fixture.request(t, http.MethodPost, "/api/v1/files/operations", map[string]any{
		"operation": "delete", "source": "docs/",
	}, "application/json")
	if operation.Code != http.StatusAccepted {
		t.Fatalf("directory delete = %d %s", operation.Code, operation.Body.String())
	}
	var queued struct {
		Job jobs.Job `json:"job"`
	}
	if err := json.Unmarshal(operation.Body.Bytes(), &queued); err != nil || queued.Job.ID == "" {
		t.Fatalf("queued job = %#v, %v", queued, err)
	}
	jobResponse := fixture.request(t, http.MethodGet, "/api/v1/jobs/"+queued.Job.ID, nil, "")
	if jobResponse.Code != http.StatusOK || !strings.Contains(jobResponse.Body.String(), queued.Job.ID) {
		t.Fatalf("job response = %d %s", jobResponse.Code, jobResponse.Body.String())
	}
}

func TestFilesAPIDoesNotInlineHTML(t *testing.T) {
	t.Parallel()
	fixture := newFilesAPIFixture(t)
	uploaded := fixture.request(t, http.MethodPut, "/api/v1/files/content?key=page.html", []byte("<script>alert(1)</script>"), "text/html")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload = %d %s", uploaded.Code, uploaded.Body.String())
	}
	preview := fixture.request(t, http.MethodGet, "/api/v1/files/content?key=page.html&mode=preview", nil, "")
	if preview.Code != http.StatusUnsupportedMediaType || strings.Contains(preview.Body.String(), "<script>") {
		t.Fatalf("html preview = %d %s", preview.Code, preview.Body.String())
	}
}

type filesAPIFixture struct {
	handler http.Handler
	cookie  *http.Cookie
	csrf    string
}

func newFilesAPIFixture(t *testing.T) filesAPIFixture {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	authStore := auth.NewStore(db)
	if err := authStore.InitializeAdmin(context.Background(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{16}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
		R2AccessKeyID: "access", R2SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	index := r2.NewStore(db, r2.Limits{StorageBytes: 1 << 30, ClassA: 10000, ClassB: 10000})
	if _, err := index.CreateBucket(context.Background(), r2.CreateBucketInput{AccountID: account.ID, Name: "physical"}); err != nil {
		t.Fatal(err)
	}
	service := &r2.Service{Index: index, Accounts: accountStore, Backend: &filesAPIBackend{objects: make(map[string][]byte)}, TempDir: t.TempDir()}
	jobStore := jobs.NewStore(db)
	handler := New(Dependencies{
		DB: db, Auth: authStore, Accounts: accountStore, Jobs: jobStore, Audit: audit.NewStore(db),
		R2: index, R2Service: service, Version: "test",
	})
	login := performJSON(t, handler, http.MethodPost, "/api/v1/session", map[string]string{"password": "correct horse battery staple"}, nil)
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	return filesAPIFixture{handler: handler, cookie: login.Result().Cookies()[0], csrf: session.CSRF}
}

func (f filesAPIFixture) request(t *testing.T, method, path string, body any, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	switch value := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(value)
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	request.AddCookie(f.cookie)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("X-CSRF-Token", f.csrf)
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

type filesAPIBackend struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (b *filesAPIBackend) Put(_ context.Context, target r2.Target, key string, body io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, _ := io.ReadAll(body)
	b.objects[target.Bucket+"/"+key] = data
	return "etag", nil
}

func (b *filesAPIBackend) Get(_ context.Context, target r2.Target, key string, _ r2.GetOptions) (r2.GetResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objects[target.Bucket+"/"+key]
	if !ok {
		return r2.GetResult{}, r2.ErrObjectNotFound
	}
	copyOfData := append([]byte(nil), data...)
	return r2.GetResult{Body: io.NopCloser(bytes.NewReader(copyOfData)), Size: int64(len(copyOfData)), ETag: "etag"}, nil
}

func (b *filesAPIBackend) Delete(_ context.Context, target r2.Target, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, target.Bucket+"/"+key)
	return nil
}
