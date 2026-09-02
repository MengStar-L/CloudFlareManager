package r2

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

type recordingAWSClient struct {
	request  *http.Request
	response func(*http.Request) *http.Response
}

func (c *recordingAWSClient) Do(request *http.Request) (*http.Response, error) {
	c.request = request
	return c.response(request), nil
}

func TestAWSBackendSendsConditionalPutHeaders(t *testing.T) {
	t.Parallel()
	client := &recordingAWSClient{response: func(request *http.Request) *http.Response {
		return awsTestResponse(request, http.StatusOK, "", http.Header{"Etag": {`"new-etag"`}})
	}}
	backend := AWSBackend{HTTPClient: client}
	got, err := backend.Put(context.Background(), awsTestTarget(), "catalog.json", strings.NewReader("data"), 4,
		"application/json", nil, PutOptions{IfMatch: `"old-etag"`, IfNoneMatch: `"other-etag"`})
	if err != nil {
		t.Fatal(err)
	}
	if got != "new-etag" {
		t.Fatalf("ETag = %q", got)
	}
	if client.request == nil || client.request.Header.Get("If-Match") != `"old-etag"` ||
		client.request.Header.Get("If-None-Match") != `"other-etag"` {
		t.Fatalf("conditional headers = %#v", client.request.Header)
	}
}

func TestAWSBackendSendsConditionalMultipartCompletionHeaders(t *testing.T) {
	t.Parallel()
	client := &recordingAWSClient{response: func(request *http.Request) *http.Response {
		body := `<CompleteMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
			`<Bucket>bucket</Bucket><Key>catalog.bin</Key><ETag>"complete-etag"</ETag></CompleteMultipartUploadResult>`
		return awsTestResponse(request, http.StatusOK, body, http.Header{"Content-Type": {"application/xml"}})
	}}
	backend := AWSBackend{HTTPClient: client}
	got, err := backend.CompleteMultipart(context.Background(), awsTestTarget(), "catalog.bin", "upload-id",
		[]CompletedPart{{PartNumber: 1, ETag: "part-etag"}}, PutOptions{IfNoneMatch: "*"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "complete-etag" {
		t.Fatalf("ETag = %q", got)
	}
	if client.request == nil || client.request.Header.Get("If-None-Match") != "*" || client.request.Header.Get("If-Match") != "" {
		t.Fatalf("conditional headers = %#v", client.request.Header)
	}
}

func TestAWSBackendDeletesObjectBatch(t *testing.T) {
	t.Parallel()
	type deleteRequest struct {
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	client := &recordingAWSClient{response: func(request *http.Request) *http.Response {
		if request.Method != http.MethodPost || !request.URL.Query().Has("delete") {
			t.Fatalf("delete request = %s %s", request.Method, request.URL.String())
		}
		var body deleteRequest
		if err := xml.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode delete request: %v", err)
		}
		if len(body.Objects) != 2 || body.Objects[0].Key != "alpha.txt" || body.Objects[1].Key != "nested/beta.txt" {
			t.Fatalf("delete objects = %#v", body.Objects)
		}
		response := `<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
			`<Deleted><Key>alpha.txt</Key></Deleted><Deleted><Key>nested/beta.txt</Key></Deleted></DeleteResult>`
		return awsTestResponse(request, http.StatusOK, response, http.Header{"Content-Type": {"application/xml"}})
	}}
	deleted, err := (AWSBackend{HTTPClient: client}).DeleteRemoteBatch(
		context.Background(), awsTestTarget(), []string{"alpha.txt", "nested/beta.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
}

func TestAWSBackendReportsPartialBatchDeleteErrors(t *testing.T) {
	t.Parallel()
	client := &recordingAWSClient{response: func(request *http.Request) *http.Response {
		response := `<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
			`<Deleted><Key>removed.txt</Key></Deleted>` +
			`<Error><Key>locked.txt</Key><Code>AccessDenied</Code><Message>object is locked</Message></Error>` +
			`</DeleteResult>`
		return awsTestResponse(request, http.StatusOK, response, http.Header{"Content-Type": {"application/xml"}})
	}}
	deleted, err := (AWSBackend{HTTPClient: client}).DeleteRemoteBatch(
		context.Background(), awsTestTarget(), []string{"removed.txt", "locked.txt"})
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	var batchErr *RemoteBatchDeleteError
	if !errors.As(err, &batchErr) {
		t.Fatalf("error = %v, want RemoteBatchDeleteError", err)
	}
	if len(batchErr.Failures) != 1 || batchErr.Failures[0].Key != "locked.txt" || batchErr.Failures[0].Code != "AccessDenied" {
		t.Fatalf("failures = %#v", batchErr.Failures)
	}
	if !strings.Contains(err.Error(), "locked.txt") || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("error does not report failed key and code: %v", err)
	}
}

func TestAWSBackendRejectsInvalidDeleteBatch(t *testing.T) {
	t.Parallel()
	backend := AWSBackend{}
	for _, size := range []int{0, 1001} {
		_, err := backend.DeleteRemoteBatch(context.Background(), awsTestTarget(), make([]string, size))
		if err == nil {
			t.Fatalf("batch size %d accepted", size)
		}
	}
}

func TestAWSBackendListsRemoteMultipart(t *testing.T) {
	t.Parallel()
	client := &recordingAWSClient{response: func(request *http.Request) *http.Response {
		query := request.URL.Query()
		if request.Method != http.MethodGet || !query.Has("uploads") {
			t.Fatalf("multipart request = %s %s", request.Method, request.URL.String())
		}
		if query.Get("key-marker") != "archive.bin" || query.Get("upload-id-marker") != "upload-1" ||
			query.Get("max-uploads") != strconv.Itoa(25) {
			t.Fatalf("multipart query = %v", query)
		}
		response := `<ListMultipartUploadsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
			`<Bucket>bucket</Bucket><IsTruncated>true</IsTruncated>` +
			`<Upload><Key>archive.bin</Key><UploadId>upload-2</UploadId></Upload>` +
			`<NextKeyMarker>next.bin</NextKeyMarker><NextUploadIdMarker>upload-9</NextUploadIdMarker>` +
			`</ListMultipartUploadsResult>`
		return awsTestResponse(request, http.StatusOK, response, http.Header{"Content-Type": {"application/xml"}})
	}}
	page, err := (AWSBackend{HTTPClient: client}).ListRemoteMultipart(
		context.Background(), awsTestTarget(), "archive.bin", "upload-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Uploads) != 1 || page.Uploads[0].Key != "archive.bin" || page.Uploads[0].UploadID != "upload-2" {
		t.Fatalf("uploads = %#v", page.Uploads)
	}
	if !page.Truncated || page.NextKeyMarker != "next.bin" || page.NextUploadIDMarker != "upload-9" {
		t.Fatalf("page = %#v", page)
	}
}

func TestClassifyAWSMutationError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "precondition status", err: awsTestError{code: "Other", status: http.StatusPreconditionFailed}, want: ErrConditionalRequestConflict},
		{name: "conditional conflict code", err: awsTestError{code: "ConditionalRequestConflict"}, want: ErrConditionalRequestConflict},
		{name: "rate status", err: awsTestError{code: "Other", status: http.StatusTooManyRequests}, want: ErrRateLimited},
		{name: "slow down code", err: awsTestError{code: "SlowDown"}, want: ErrRateLimited},
		{name: "range status", err: awsTestError{code: "Other", status: http.StatusRequestedRangeNotSatisfiable}, want: ErrRangeNotSatisfiable},
		{name: "invalid range code", err: awsTestError{code: "InvalidRange"}, want: ErrRangeNotSatisfiable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := classifyAWSMutationError(test.err)
			if !errors.Is(err, test.want) {
				t.Fatalf("classified error = %v, want %v", err, test.want)
			}
			var original awsTestError
			if !errors.As(err, &original) || original.code != test.err.(awsTestError).code || original.status != test.err.(awsTestError).status {
				t.Fatalf("classified error lost original AWS error: %v", err)
			}
		})
	}
}

func TestAWSBackendClassifiesInvalidGetRange(t *testing.T) {
	t.Parallel()
	client := &recordingAWSClient{response: func(request *http.Request) *http.Response {
		body := `<Error><Code>InvalidRange</Code><Message>The requested range is not satisfiable</Message></Error>`
		return awsTestResponse(request, http.StatusRequestedRangeNotSatisfiable, body, http.Header{"Content-Type": {"application/xml"}})
	}}
	backend := AWSBackend{HTTPClient: client}
	_, err := backend.Get(context.Background(), awsTestTarget(), "catalog.json", GetOptions{Range: "bytes=100-"})
	if !errors.Is(err, ErrRangeNotSatisfiable) {
		t.Fatalf("classified error = %v", err)
	}
}

func TestAWSBackendClassifiesConditionalResponse(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusPreconditionFailed, http.StatusConflict} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			code := "PreconditionFailed"
			if status == http.StatusConflict {
				code = "ConditionalRequestConflict"
			}
			client := &recordingAWSClient{response: func(request *http.Request) *http.Response {
				body := `<Error><Code>` + code + `</Code><Message>condition failed</Message></Error>`
				return awsTestResponse(request, status, body, http.Header{"Content-Type": {"application/xml"}})
			}}
			backend := AWSBackend{HTTPClient: client}
			_, err := backend.Put(context.Background(), awsTestTarget(), "catalog.json", strings.NewReader("data"), 4,
				"application/json", nil, PutOptions{IfNoneMatch: "*"})
			if !errors.Is(err, ErrConditionalRequestConflict) {
				t.Fatalf("classified error = %v", err)
			}
		})
	}
}

type awsTestError struct {
	code   string
	status int
}

func (e awsTestError) Error() string       { return e.code }
func (e awsTestError) ErrorCode() string   { return e.code }
func (e awsTestError) HTTPStatusCode() int { return e.status }

func awsTestTarget() Target {
	return Target{
		CloudflareAccountID: "account", AccessKeyID: "access", SecretAccessKey: "secret", Bucket: "bucket",
	}
}

func awsTestResponse(request *http.Request, status int, body string, headers http.Header) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}
