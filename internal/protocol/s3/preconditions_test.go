package s3protocol

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
)

type recordingReadObjects struct {
	*stubObjects
	getCalls int
	options  r2.GetOptions
}

func (s *recordingReadObjects) Get(ctx context.Context, key string, options r2.GetOptions) (r2.GetResult, error) {
	s.getCalls++
	s.options = options
	return s.stubObjects.Get(ctx, key, options)
}

func TestS3ReadPreconditionsForGetAndHead(t *testing.T) {
	t.Parallel()
	modified := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		method := method
		for _, test := range []struct {
			name       string
			headers    http.Header
			wantStatus int
			wantGet    bool
		}{
			{name: "stale if-match", headers: http.Header{"If-Match": {`"stale"`}}, wantStatus: http.StatusPreconditionFailed},
			{name: "weak if-match", headers: http.Header{"If-Match": {`W/"current"`}}, wantStatus: http.StatusPreconditionFailed},
			{name: "matching if-none-match", headers: http.Header{"If-None-Match": {`W/"other", W/"current"`}}, wantStatus: http.StatusNotModified},
			{name: "not modified since", headers: http.Header{"If-Modified-Since": {modified.Format(http.TimeFormat)}}, wantStatus: http.StatusNotModified},
			{
				name: "if-match takes precedence over stale date",
				headers: http.Header{
					"If-Match":            {`"current"`},
					"If-Unmodified-Since": {modified.Add(-time.Hour).Format(http.TimeFormat)},
				},
				wantStatus: http.StatusOK, wantGet: method == http.MethodGet,
			},
			{
				name: "if-none-match takes precedence over modified date",
				headers: http.Header{
					"If-None-Match":     {`"different"`},
					"If-Modified-Since": {modified.Add(time.Hour).Format(http.TimeFormat)},
				},
				wantStatus: http.StatusOK, wantGet: method == http.MethodGet,
			},
		} {
			t.Run(method+"/"+test.name, func(t *testing.T) {
				objects := newRecordingReadObjects(modified)
				handler := Handler{Objects: objects}
				request := httptest.NewRequest(method, "/storage/object.txt", nil)
				request.Header = test.headers.Clone()
				response := httptest.NewRecorder()
				if method == http.MethodGet {
					handler.getObject(response, request, "request-id", "object.txt")
				} else {
					handler.headObject(response, request, "request-id", "object.txt")
				}
				if response.Code != test.wantStatus {
					t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
				}
				if (objects.getCalls != 0) != test.wantGet {
					t.Fatalf("physical GET calls = %d, wantGet = %v", objects.getCalls, test.wantGet)
				}
				if got := response.Header().Get("ETag"); got != `"current"` {
					t.Fatalf("ETag = %q", got)
				}
				if test.wantStatus == http.StatusNotModified && response.Body.Len() != 0 {
					t.Fatalf("304 body = %q", response.Body.String())
				}
				if method == http.MethodHead && response.Body.Len() != 0 {
					t.Fatalf("HEAD body = %q", response.Body.String())
				}
			})
		}
	}
}

func TestS3IfRangeControlsPhysicalRange(t *testing.T) {
	t.Parallel()
	modified := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		ifRange   string
		wantRange string
	}{
		{name: "matching strong tag", ifRange: `"current"`, wantRange: "bytes=0-1"},
		{name: "stale tag", ifRange: `"stale"`},
		{name: "weak tag", ifRange: `W/"current"`},
		{name: "fresh date", ifRange: modified.Format(http.TimeFormat), wantRange: "bytes=0-1"},
		{name: "stale date", ifRange: modified.Add(-time.Second).Format(http.TimeFormat)},
		{name: "future date", ifRange: modified.Add(time.Second).Format(http.TimeFormat)},
		{name: "invalid", ifRange: "not-a-validator"},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := newRecordingReadObjects(modified)
			handler := Handler{Objects: objects}
			request := httptest.NewRequest(http.MethodGet, "/storage/object.txt", nil)
			request.Header.Set("Range", "bytes=0-1")
			request.Header.Set("If-Range", test.ifRange)
			response := httptest.NewRecorder()
			handler.getObject(response, request, "request-id", "object.txt")
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if objects.options.Range != test.wantRange {
				t.Fatalf("physical Range = %q, want %q", objects.options.Range, test.wantRange)
			}
			if objects.options.IfMatch != `"current"` || objects.options.IfNoneMatch != "" ||
				objects.options.IfModifiedSince != nil || objects.options.IfUnmodifiedSince != nil {
				t.Fatalf("physical GET conditions = %#v", objects.options)
			}
		})
	}
}

func TestS3EntityTagListAllowsQuotedComma(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/storage/object.txt", nil)
	request.Header.Set("If-None-Match", `"first,part", W/"current"`)
	evaluation, err := evaluateReadConditions(request, r2.Object{ETag: "current"})
	if err != nil || evaluation.status != http.StatusNotModified {
		t.Fatalf("evaluation = %#v, error = %v", evaluation, err)
	}
}

func TestS3MalformedEntityTagConditionIsRejected(t *testing.T) {
	t.Parallel()
	objects := newRecordingReadObjects(time.Now())
	handler := Handler{Objects: objects}
	request := httptest.NewRequest(http.MethodGet, "/storage/object.txt", nil)
	request.Header.Set("If-Match", `"unterminated`)
	response := httptest.NewRecorder()
	handler.getObject(response, request, "request-id", "object.txt")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "InvalidArgument") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if objects.getCalls != 0 {
		t.Fatalf("physical GET calls = %d", objects.getCalls)
	}
}

type changingReadObjects struct {
	*stubObjects
	statCalls int
	getCalls  int
}

func (s *changingReadObjects) Stat(context.Context, string) (r2.Object, error) {
	s.statCalls++
	etag := "first"
	if s.statCalls > 1 {
		etag = "second"
	}
	return r2.Object{Key: "object.txt", Size: 6, ETag: etag, LastModified: time.Now()}, nil
}

func (s *changingReadObjects) Get(_ context.Context, _ string, options r2.GetOptions) (r2.GetResult, error) {
	s.getCalls++
	if options.IfMatch == `"first"` {
		return r2.GetResult{}, r2.ErrConditionalRequestConflict
	}
	return r2.GetResult{
		Body: io.NopCloser(strings.NewReader("second")), Size: 6, ETag: "second",
	}, nil
}

func TestS3GetReevaluatesConditionsWhenLogicalVersionChanges(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		ifMatch    string
		wantStatus int
		wantBody   string
		wantGets   int
	}{
		{name: "conditional request fails against replacement", ifMatch: `"first"`, wantStatus: http.StatusPreconditionFailed, wantGets: 1},
		{name: "unconditional request retries replacement", wantStatus: http.StatusOK, wantBody: "second", wantGets: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := &changingReadObjects{stubObjects: &stubObjects{}}
			handler := Handler{Objects: objects}
			request := httptest.NewRequest(http.MethodGet, "/storage/object.txt", nil)
			if test.ifMatch != "" {
				request.Header.Set("If-Match", test.ifMatch)
			}
			response := httptest.NewRecorder()
			handler.getObject(response, request, "request-id", "object.txt")
			if response.Code != test.wantStatus {
				t.Fatalf("response = %d %q", response.Code, response.Body.String())
			}
			if test.wantBody != "" && response.Body.String() != test.wantBody {
				t.Fatalf("body = %q, want %q", response.Body.String(), test.wantBody)
			}
			if test.wantStatus == http.StatusPreconditionFailed && !strings.Contains(response.Body.String(), "PreconditionFailed") {
				t.Fatalf("precondition response body = %q", response.Body.String())
			}
			if objects.getCalls != test.wantGets {
				t.Fatalf("GET calls = %d, want %d", objects.getCalls, test.wantGets)
			}
		})
	}
}

type sameETagReplacingReadObjects struct {
	*stubObjects
	modified  time.Time
	statCalls int
	getCalls  int
	ranges    []string
}

func (s *sameETagReplacingReadObjects) Stat(context.Context, string) (r2.Object, error) {
	s.statCalls++
	objectID := "first-object"
	modified := s.modified
	if s.statCalls > 1 {
		objectID = "second-object"
		modified = modified.Add(time.Hour)
	}
	return r2.Object{
		Key: "object.txt", ObjectID: objectID, Size: 6, ETag: "shared", LastModified: modified,
	}, nil
}

func (s *sameETagReplacingReadObjects) Get(_ context.Context, _ string, options r2.GetOptions) (r2.GetResult, error) {
	s.getCalls++
	s.ranges = append(s.ranges, options.Range)
	if options.ExpectedObjectID == "first-object" {
		return r2.GetResult{}, r2.ErrConditionalRequestConflict
	}
	if options.ExpectedObjectID != "second-object" {
		return r2.GetResult{}, r2.ErrConditionalRequestConflict
	}
	return r2.GetResult{
		Body: io.NopCloser(strings.NewReader("second")), Size: 6, ETag: "shared",
		LastModified: s.modified.Add(time.Hour),
	}, nil
}

func TestS3GetReevaluatesSameETagLogicalReplacement(t *testing.T) {
	t.Parallel()
	modified := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		headers    http.Header
		wantStatus int
		wantBody   string
		wantGets   int
		wantRanges []string
	}{
		{
			name: "date precondition is reevaluated", headers: http.Header{
				"If-Unmodified-Since": {modified.Format(http.TimeFormat)},
			},
			wantStatus: http.StatusPreconditionFailed, wantGets: 1, wantRanges: []string{""},
		},
		{
			name: "if-range date is reevaluated", headers: http.Header{
				"Range": {"bytes=0-1"}, "If-Range": {modified.Format(http.TimeFormat)},
			},
			wantStatus: http.StatusOK, wantBody: "second", wantGets: 2,
			wantRanges: []string{"bytes=0-1", ""},
		},
		{
			name: "unconditional request retries", wantStatus: http.StatusOK, wantBody: "second",
			wantGets: 2, wantRanges: []string{"", ""},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := &sameETagReplacingReadObjects{stubObjects: &stubObjects{}, modified: modified}
			handler := Handler{Objects: objects}
			request := httptest.NewRequest(http.MethodGet, "/storage/object.txt", nil)
			request.Header = test.headers.Clone()
			response := httptest.NewRecorder()
			handler.getObject(response, request, "request-id", "object.txt")
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody && test.wantBody != "" {
				t.Fatalf("response = %d %q, want %d %q", response.Code, response.Body.String(), test.wantStatus, test.wantBody)
			}
			if objects.getCalls != test.wantGets {
				t.Fatalf("GET calls = %d, want %d", objects.getCalls, test.wantGets)
			}
			if strings.Join(objects.ranges, ",") != strings.Join(test.wantRanges, ",") {
				t.Fatalf("physical ranges = %#v, want %#v", objects.ranges, test.wantRanges)
			}
		})
	}
}

func newRecordingReadObjects(modified time.Time) *recordingReadObjects {
	return &recordingReadObjects{stubObjects: &stubObjects{
		values: map[string][]byte{"object.txt": []byte("data")},
		metadata: map[string]r2.Object{
			"object.txt": {
				Key: "object.txt", Size: 4, ETag: "current", ContentType: "text/plain",
				LastModified: modified,
			},
		},
	}}
}

var _ ObjectService = (*recordingReadObjects)(nil)
var _ ObjectService = (*changingReadObjects)(nil)
var _ ObjectService = (*sameETagReplacingReadObjects)(nil)
