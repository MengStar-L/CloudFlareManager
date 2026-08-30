package r2

import (
	"context"
	"errors"
	"io"
	"net/http"
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
