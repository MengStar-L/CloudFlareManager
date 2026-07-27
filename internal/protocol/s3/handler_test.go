package s3protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	awstypes "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
)

func TestHandlerPutHeadGet(t *testing.T) {
	t.Parallel()

	const accessKey = "CFR2EXAMPLE"
	const secretKey = "super-secret-value"
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	objects := &stubObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}
	handler := Handler{
		Bucket: "storage", Objects: objects,
		Auth: Authenticator{
			Now: func() time.Time { return now },
			Resolve: func(context.Context, string) (Identity, string, error) {
				return Identity{PublicID: accessKey, Scopes: []string{"r2:*"}}, secretKey, nil
			},
		},
	}

	put := signedRequest(t, http.MethodPut, "http://localhost/storage/docs/readme.txt", []byte("hello"), accessKey, secretKey, now)
	putResponse := httptest.NewRecorder()
	handler.ServeHTTP(putResponse, put)
	if putResponse.Code != http.StatusOK {
		t.Fatalf("put status = %d, authorization = %q, body = %s", putResponse.Code, put.Header.Get("Authorization"), putResponse.Body.String())
	}

	head := signedRequest(t, http.MethodHead, "http://localhost/storage/docs/readme.txt", nil, accessKey, secretKey, now)
	headResponse := httptest.NewRecorder()
	handler.ServeHTTP(headResponse, head)
	if headResponse.Code != http.StatusOK || headResponse.Header().Get("Content-Length") != "5" {
		t.Fatalf("head response = %d %#v", headResponse.Code, headResponse.Header())
	}

	get := signedRequest(t, http.MethodGet, "http://localhost/storage/docs/readme.txt", nil, accessKey, secretKey, now)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK || getResponse.Body.String() != "hello" {
		t.Fatalf("get response = %d %q", getResponse.Code, getResponse.Body.String())
	}
}

func TestHandlerListObjectsV2DelimiterPagination(t *testing.T) {
	t.Parallel()

	const accessKey = "CFR2EXAMPLE"
	const secretKey = "super-secret-value"
	now := time.Date(2026, 7, 25, 9, 15, 0, 0, time.UTC)
	objects := &stubObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}
	for _, key := range []string{"docs/a.txt", "docs/b.txt", "images/a.png", "root.txt"} {
		objects.metadata[key] = r2.Object{Key: key, Size: 1, ETag: key, LastModified: now}
	}
	handler := testHandler(objects, accessKey, secretKey, now)

	first := signedRequest(t, http.MethodGet, "http://localhost/storage?list-type=2&delimiter=%2F&max-keys=2", nil, accessKey, secretKey, now)
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, first)
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first list status = %d, body = %s", firstResponse.Code, firstResponse.Body.String())
	}
	var firstResult listBucketResult
	if err := xml.Unmarshal(firstResponse.Body.Bytes(), &firstResult); err != nil {
		t.Fatal(err)
	}
	if !firstResult.IsTruncated || len(firstResult.CommonPrefixes) != 2 || firstResult.CommonPrefixes[0].Prefix != "docs/" {
		t.Fatalf("first list = %#v", firstResult)
	}
	if firstResult.NextContinuationToken == "" {
		t.Fatal("expected continuation token")
	}

	second := signedRequest(t, http.MethodGet, "http://localhost/storage?list-type=2&delimiter=%2F&continuation-token="+url.QueryEscape(firstResult.NextContinuationToken), nil, accessKey, secretKey, now)
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, second)
	var secondResult listBucketResult
	if err := xml.Unmarshal(secondResponse.Body.Bytes(), &secondResult); err != nil {
		t.Fatal(err)
	}
	if secondResponse.Code != http.StatusOK || len(secondResult.Contents) != 1 || secondResult.Contents[0].Key != "root.txt" || len(secondResult.CommonPrefixes) != 0 {
		t.Fatalf("second list status = %d, result = %#v", secondResponse.Code, secondResult)
	}
}

func TestHandlerHidesWebDAVNamespace(t *testing.T) {
	t.Parallel()
	const accessKey = "CFR2EXAMPLE"
	const secretKey = "super-secret-value"
	now := time.Date(2026, 7, 25, 9, 20, 0, 0, time.UTC)
	objects := &stubObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}
	objects.metadata["public.txt"] = r2.Object{Key: "public.txt", Size: 1, ETag: "public", LastModified: now}
	internal := r2.WebDAVMountPrefix("credential-id") + "private.txt"
	objects.metadata[internal] = r2.Object{Key: internal, Size: 1, ETag: "private", LastModified: now}
	objects.values[internal] = []byte("x")
	handler := testHandler(objects, accessKey, secretKey, now)

	list := signedRequest(t, http.MethodGet, "http://localhost/storage?list-type=2", nil, accessKey, secretKey, now)
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, list)
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), "public.txt") || strings.Contains(listResponse.Body.String(), r2.WebDAVNamespaceRoot) {
		t.Fatalf("list = %d %s", listResponse.Code, listResponse.Body.String())
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete} {
		request := signedRequest(t, method, "http://localhost/storage/"+internal, nil, accessKey, secretKey, now)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s reserved status = %d", method, response.Code)
		}
	}
}

func TestHandlerMultipartLifecycle(t *testing.T) {
	t.Parallel()

	const accessKey = "CFR2EXAMPLE"
	const secretKey = "super-secret-value"
	now := time.Date(2026, 7, 25, 9, 30, 0, 0, time.UTC)
	objects := &multipartStub{stubObjects: &stubObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}}
	handler := testHandler(objects, accessKey, secretKey, now)

	initiate := signedRequest(t, http.MethodPost, "http://localhost/storage/large.bin?uploads", nil, accessKey, secretKey, now)
	initiateResponse := httptest.NewRecorder()
	handler.ServeHTTP(initiateResponse, initiate)
	if initiateResponse.Code != http.StatusOK || !strings.Contains(initiateResponse.Body.String(), "upload-1") {
		t.Fatalf("initiate response = %d %s", initiateResponse.Code, initiateResponse.Body.String())
	}

	upload := signedRequest(t, http.MethodPut, "http://localhost/storage/large.bin?partNumber=1&uploadId=upload-1", []byte("hello"), accessKey, secretKey, now)
	uploadResponse := httptest.NewRecorder()
	handler.ServeHTTP(uploadResponse, upload)
	if uploadResponse.Code != http.StatusOK || uploadResponse.Header().Get("ETag") != `"part-etag"` {
		t.Fatalf("upload response = %d %#v", uploadResponse.Code, uploadResponse.Header())
	}

	completeBody := []byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"part-etag"</ETag></Part></CompleteMultipartUpload>`)
	complete := signedRequest(t, http.MethodPost, "http://localhost/storage/large.bin?uploadId=upload-1", completeBody, accessKey, secretKey, now)
	completeResponse := httptest.NewRecorder()
	handler.ServeHTTP(completeResponse, complete)
	if completeResponse.Code != http.StatusOK || !strings.Contains(completeResponse.Body.String(), "multipart-etag") {
		t.Fatalf("complete response = %d %s", completeResponse.Code, completeResponse.Body.String())
	}
}

func TestAWSGoSDKMultipartContract(t *testing.T) {
	t.Parallel()

	const accessKey = "CFR2EXAMPLE"
	const secretKey = "super-secret-value"
	objects := &multipartStub{stubObjects: &stubObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}}
	handler := Handler{
		Bucket: "storage", Objects: objects,
		Auth: Authenticator{
			Resolve: func(context.Context, string) (Identity, string, error) {
				return Identity{PublicID: accessKey, Scopes: []string{"r2:*"}}, secretKey, nil
			},
		},
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := awss3.NewFromConfig(aws.Config{
		Region: "auto", Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
	}, func(options *awss3.Options) {
		options.BaseEndpoint = aws.String(server.URL)
		options.UsePathStyle = true
	})

	created, err := client.CreateMultipartUpload(context.Background(), &awss3.CreateMultipartUploadInput{
		Bucket: aws.String("storage"), Key: aws.String("sdk.bin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	uploaded, err := client.UploadPart(context.Background(), &awss3.UploadPartInput{
		Bucket: aws.String("storage"), Key: aws.String("sdk.bin"), UploadId: created.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader([]byte("sdk-data")), ContentLength: aws.Int64(8),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CompleteMultipartUpload(context.Background(), &awss3.CompleteMultipartUploadInput{
		Bucket: aws.String("storage"), Key: aws.String("sdk.bin"), UploadId: created.UploadId,
		MultipartUpload: &awstypes.CompletedMultipartUpload{Parts: []awstypes.CompletedPart{{
			PartNumber: aws.Int32(1), ETag: uploaded.ETag,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.GetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: aws.String("storage"), Key: aws.String("sdk.bin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()
	if string(data) != "sdk-data" {
		t.Fatalf("body = %q", data)
	}
}

func testHandler(objects ObjectService, accessKey, secretKey string, now time.Time) Handler {
	return Handler{
		Bucket: "storage", Objects: objects,
		Auth: Authenticator{
			Now: func() time.Time { return now },
			Resolve: func(context.Context, string) (Identity, string, error) {
				return Identity{PublicID: accessKey, Scopes: []string{"r2:*"}}, secretKey, nil
			},
		},
	}
}

func signedRequest(t *testing.T, method, target string, body []byte, accessKey, secretKey string, now time.Time) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(digest[:])
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if err := awsv4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey}, request, payloadHash, "s3", "auto", now); err != nil {
		t.Fatal(err)
	}
	return request
}

type stubObjects struct {
	values   map[string][]byte
	metadata map[string]r2.Object
}

func (s *stubObjects) Put(_ context.Context, request r2.PutRequest) (r2.Object, error) {
	data, _ := io.ReadAll(request.Body)
	s.values[request.Key] = data
	object := r2.Object{Key: request.Key, Size: int64(len(data)), ETag: "etag", ContentType: request.ContentType, Metadata: request.Metadata, LastModified: time.Now()}
	s.metadata[request.Key] = object
	return object, nil
}

func (s *stubObjects) Get(_ context.Context, key string, _ r2.GetOptions) (r2.GetResult, error) {
	data, found := s.values[key]
	if !found {
		return r2.GetResult{}, r2.ErrObjectNotFound
	}
	object := s.metadata[key]
	return r2.GetResult{Body: io.NopCloser(bytes.NewReader(data)), Size: int64(len(data)), ETag: object.ETag, ContentType: object.ContentType}, nil
}

func (s *stubObjects) Stat(_ context.Context, key string) (r2.Object, error) {
	object, found := s.metadata[key]
	if !found {
		return r2.Object{}, r2.ErrObjectNotFound
	}
	return object, nil
}

func (s *stubObjects) List(_ context.Context, options r2.ListOptions) (r2.ObjectList, error) {
	keys := make([]string, 0, len(s.metadata))
	for key := range s.metadata {
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

type multipartStub struct {
	*stubObjects
	part []byte
}

func (s *multipartStub) CreateMultipart(_ context.Context, input r2.CreateMultipartInput) (r2.MultipartUpload, error) {
	return r2.MultipartUpload{ID: "upload-1", Key: input.Key, CreatedAt: time.Now()}, nil
}

func (s *multipartStub) UploadPart(_ context.Context, request r2.UploadPartRequest) (r2.MultipartPart, error) {
	s.part, _ = io.ReadAll(request.Body)
	return r2.MultipartPart{PartNumber: request.PartNumber, ETag: "part-etag", Size: int64(len(s.part)), LastModified: time.Now()}, nil
}

func (s *multipartStub) ListParts(context.Context, string, string, int32, int) (r2.MultipartPartList, error) {
	return r2.MultipartPartList{Parts: []r2.MultipartPart{{PartNumber: 1, ETag: "part-etag", Size: int64(len(s.part)), LastModified: time.Now()}}}, nil
}

func (s *multipartStub) CompleteMultipart(_ context.Context, request r2.CompleteMultipartRequest) (r2.Object, error) {
	s.values[request.Key] = append([]byte(nil), s.part...)
	object := r2.Object{Key: request.Key, Size: int64(len(s.part)), ETag: "multipart-etag", LastModified: time.Now()}
	s.metadata[request.Key] = object
	return object, nil
}

func (s *multipartStub) AbortMultipart(context.Context, string, string) error { return nil }

func (s *multipartStub) ListMultipart(context.Context, r2.ListMultipartOptions) (r2.MultipartUploadList, error) {
	return r2.MultipartUploadList{}, nil
}

func (s *stubObjects) Delete(_ context.Context, key string) error {
	delete(s.values, key)
	delete(s.metadata, key)
	return nil
}

func (s *stubObjects) Copy(_ context.Context, source, destination string) (r2.Object, error) {
	data, found := s.values[source]
	if !found {
		return r2.Object{}, r2.ErrObjectNotFound
	}
	s.values[destination] = append([]byte(nil), data...)
	object := s.metadata[source]
	object.Key = destination
	s.metadata[destination] = object
	return object, nil
}
