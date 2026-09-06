package webdavprotocol

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func newServiceDAVHandler(t *testing.T) (Handler, r2.Service) {
	t.Helper()
	locks, db := newTestLockStore(t)
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{11}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "account", APIToken: "token",
		R2AccessKeyID: "access", R2SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	index := r2.NewStore(db, r2.Limits{StorageBytes: 1 << 20, ClassA: 1000, ClassB: 1000})
	bucket, err := index.CreateBucket(context.Background(), r2.CreateBucketInput{AccountID: account.ID, Name: "physical"})
	if err != nil {
		t.Fatal(err)
	}
	if err := index.FinishBucketScan(context.Background(), bucket.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := index.EnsureWebDAVNamespaces(context.Background(), []string{"credential"}); err != nil {
		t.Fatal(err)
	}
	backend := &serviceDAVBackend{objects: &memoryObjects{values: map[string][]byte{}, metadata: map[string]r2.Object{}}}
	service := r2.Service{Index: index, Accounts: accountStore, Backend: backend, WebDAVCoordinator: locks, TempDir: t.TempDir()}
	return Handler{Objects: service, Locks: locks, Verify: func(context.Context, string, string) (Identity, error) {
		return Identity{ID: "credential", Scopes: []string{"r2:read", "r2:write"}}, nil
	}}, service
}

func TestServiceDAVFirstSave(t *testing.T) {
	for _, trailingSlash := range []bool{false, true} {
		t.Run(map[bool]string{false: "client-path", true: "trailing-slash"}[trailingSlash], func(t *testing.T) {
			handler, _ := newServiceDAVHandler(t)
			for _, path := range []string{"/GameSync", "/GameSync/catalog", "/GameSync/manifests", "/GameSync/objects"} {
				if trailingSlash {
					path += "/"
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				request := httptest.NewRequest("MKCOL", path, nil).WithContext(ctx)
				request.SetBasicAuth("dav", "test")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				cancel()
				if response.Code != http.StatusCreated {
					t.Fatalf("MKCOL %s = %d, body = %s", path, response.Code, response.Body.String())
				}
			}
			put := performDAVRequest(handler, http.MethodPut, "/GameSync/catalog/catalog.json", "{}", map[string]string{"If-None-Match": "*"})
			if put.Code != http.StatusCreated {
				t.Fatalf("PUT = %d", put.Code)
			}
		})
	}
}

func TestServiceDAVFileLifecycle(t *testing.T) {
	handler, _ := newServiceDAVHandler(t)
	steps := []struct {
		method, path, body string
		headers            map[string]string
		status             int
	}{
		{"MKCOL", "/GameSync", "", nil, http.StatusCreated},
		{"MKCOL", "/GameSync/catalog", "", nil, http.StatusCreated},
		{http.MethodPut, "/GameSync/catalog/catalog.json", "{}", map[string]string{"If-None-Match": "*"}, http.StatusCreated},
		{http.MethodPut, "/GameSync/catalog/catalog.json", "overwrite", map[string]string{"If-None-Match": "*"}, http.StatusPreconditionFailed},
		{"COPY", "/GameSync/catalog", "", map[string]string{"Destination": "/backup"}, http.StatusCreated},
		{"MOVE", "/backup", "", map[string]string{"Destination": "/renamed"}, http.StatusCreated},
		{http.MethodGet, "/renamed/catalog.json", "", nil, http.StatusOK},
		{http.MethodDelete, "/renamed", "", nil, http.StatusNoContent},
		{http.MethodGet, "/renamed/catalog.json", "", nil, http.StatusNotFound},
		{http.MethodGet, "/GameSync/catalog/catalog.json", "", nil, http.StatusOK},
		{http.MethodDelete, "/GameSync", "", nil, http.StatusNoContent},
	}
	for _, step := range steps {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		request := httptest.NewRequest(step.method, step.path, strings.NewReader(step.body)).WithContext(ctx)
		request.SetBasicAuth("dav", "test")
		for key, value := range step.headers {
			request.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		cancel()
		if response.Code != step.status {
			t.Fatalf("%s %s = %d, want %d; body = %s", step.method, step.path, response.Code, step.status, response.Body.String())
		}
		if step.method == http.MethodGet && response.Code == http.StatusOK && response.Body.String() != "{}" {
			t.Fatalf("%s returned %q", step.path, response.Body.String())
		}
	}
}

func TestServiceDAVRecoveryAfterUncertainWrite(t *testing.T) {
	handler, service := newServiceDAVHandler(t)
	workingBackend := service.Backend
	var methods []string
	service.Backend = r2.AWSBackend{HTTPClient: &http.Client{Transport: serviceDAVTransport(func(request *http.Request) (*http.Response, error) {
		methods = append(methods, request.Method)
		if request.Method == http.MethodPut {
			return nil, errors.New("connection interrupted after sending request")
		}
		return &http.Response{
			StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/xml"}},
			Body:    io.NopCloser(strings.NewReader(`<Error><Code>SignatureDoesNotMatch</Code><Message>Signature mismatch</Message></Error>`)),
			Request: request,
		}, nil
	})}}
	handler.Objects = service
	first := performDAVRequest(handler, "MKCOL", "/GameSync", "", nil)
	if first.Code < 400 {
		t.Fatalf("rejected upstream write returned success: %d", first.Code)
	}
	if strings.Join(methods, ",") != "PUT,HEAD" {
		t.Fatalf("upstream requests = %v", methods)
	}
	intents, err := service.Index.ListWriteIntents(context.Background(), 10)
	if err != nil || len(intents) != 1 || intents[0].State != r2.WriteRecovery {
		t.Fatalf("unresolved write intents = %#v, error = %v", intents, err)
	}
	retry := performDAVRequest(handler, "MKCOL", "/GameSync", "", nil)
	if retry.Code != http.StatusServiceUnavailable || len(methods) != 2 || !strings.Contains(retry.Body.String(), "恢复") {
		t.Fatalf("unresolved write was retried upstream: status=%d, requests=%v", retry.Code, methods)
	}
	t.Logf("first MKCOL = %d; subsequent MKCOL = %d", first.Code, retry.Code)
	if err := service.RecoverInterruptedBeforeServing(context.Background()); err == nil {
		t.Fatal("recovery unexpectedly succeeded while upstream HEAD is forbidden")
	} else {
		var pending *r2.RecoveryPendingError
		if !errors.As(err, &pending) {
			t.Fatalf("recovery would block startup: %v", err)
		}
		t.Logf("startup recovery error: %v", err)
	}
	service.Backend = workingBackend
	handler.Objects = service
	if err := service.RecoverInterruptedBeforeServing(context.Background()); err != nil {
		t.Fatalf("recovery after correcting upstream access: %v", err)
	}
	intents, err = service.Index.ListWriteIntents(context.Background(), 10)
	if err != nil || len(intents) != 0 {
		t.Fatalf("recovered write intents = %#v, error = %v", intents, err)
	}
	buckets, err := service.Index.ListBuckets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Index.FinishBucketScan(context.Background(), buckets[0].ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if response := performDAVRequest(handler, "MKCOL", "/GameSync", "", nil); response.Code != http.StatusCreated {
		t.Fatalf("MKCOL after recovery = %d", response.Code)
	}
}

func TestServiceDAVRejectedWriteDoesNotLeaveRecoveryIntent(t *testing.T) {
	handler, service := newServiceDAVHandler(t)
	requests := 0
	service.Backend = r2.AWSBackend{HTTPClient: &http.Client{Transport: serviceDAVTransport(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodPut {
			t.Fatalf("unexpected upstream request: %s", request.Method)
		}
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/xml"}},
			Body: io.NopCloser(strings.NewReader(`<Error><Code>SignatureDoesNotMatch</Code><Message>secret-value</Message></Error>`)), Request: request}, nil
	})}}
	handler.Objects = service
	for attempt := 0; attempt < 2; attempt++ {
		response := performDAVRequest(handler, "MKCOL", "/GameSync", "", nil)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "SignatureDoesNotMatch") || strings.Contains(response.Body.String(), "secret-value") {
			t.Fatalf("authentication diagnostic = %d %s", response.Code, response.Body.String())
		}
		intents, err := service.Index.ListWriteIntents(context.Background(), 10)
		if err != nil || len(intents) != 0 {
			t.Fatalf("rejected write left intents: %#v, %v", intents, err)
		}
	}
	if requests != 2 {
		t.Fatalf("upstream attempt count = %d", requests)
	}
}

type serviceDAVTransport func(*http.Request) (*http.Response, error)

func (f serviceDAVTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type serviceDAVBackend struct{ objects *memoryObjects }

func (b *serviceDAVBackend) Put(ctx context.Context, target r2.Target, key string, body io.Reader, size int64, contentType string, metadata map[string]string, options r2.PutOptions) (string, error) {
	physical := target.Bucket + "/" + key
	object, err := b.objects.Stat(ctx, physical)
	if options.IfNoneMatch == "*" && err == nil || options.IfMatch != "" && (err != nil || strings.Trim(options.IfMatch, `"`) != object.ETag) {
		return "", r2.ErrConditionalRequestConflict
	}
	stored, err := b.objects.Put(ctx, r2.PutRequest{Key: physical, Body: body, Size: size, ContentType: contentType, Metadata: metadata})
	return stored.ETag, err
}

func (b *serviceDAVBackend) Get(ctx context.Context, target r2.Target, key string, options r2.GetOptions) (r2.GetResult, error) {
	return b.objects.Get(ctx, target.Bucket+"/"+key, options)
}

func (b *serviceDAVBackend) Delete(ctx context.Context, target r2.Target, key string) error {
	return b.objects.Delete(ctx, target.Bucket+"/"+key)
}

func (b *serviceDAVBackend) Head(ctx context.Context, target r2.Target, key string) (r2.RemoteObject, error) {
	object, err := b.objects.Stat(ctx, target.Bucket+"/"+key)
	if err != nil {
		return r2.RemoteObject{}, err
	}
	return r2.RemoteObject{Key: key, Size: object.Size, ETag: object.ETag, ContentType: object.ContentType, Metadata: object.Metadata}, nil
}

func (b *serviceDAVBackend) ListRemote(context.Context, r2.Target, string, string, int32) (r2.RemoteObjectList, error) {
	return r2.RemoteObjectList{}, errors.New("unexpected ListRemote in WebDAV integration test")
}
