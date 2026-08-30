package webdavprotocol

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestStrongETag(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "opaque", value: "abc-123", want: `"abc-123"`},
		{name: "already quoted", value: `"abc-123"`, want: `"abc-123"`},
		{name: "server upgrades stored weak syntax", value: `W/"abc-123"`, want: `"abc-123"`},
		{name: "empty", value: "", want: ""},
		{name: "embedded quote", value: `abc"123`, want: ""},
		{name: "control character", value: "abc\n123", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := strongETag(test.value); got != test.want {
				t.Fatalf("strongETag(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func TestParseEntityTagSet(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		values  []string
		wantAny bool
		want    []entityTag
		valid   bool
	}{
		{name: "wildcard", values: []string{" * "}, wantAny: true, valid: true},
		{name: "strong and weak list", values: []string{`"one", W/"two"`}, want: []entityTag{{Opaque: "one"}, {Weak: true, Opaque: "two"}}, valid: true},
		{name: "repeated lines form one list", values: []string{`"one"`, `"two"`}, want: []entityTag{{Opaque: "one"}, {Opaque: "two"}}, valid: true},
		{name: "comma inside opaque tag", values: []string{`"one,two"`}, want: []entityTag{{Opaque: "one,two"}}, valid: true},
		{name: "missing quotes", values: []string{"one"}},
		{name: "wildcard plus tag", values: []string{`*, "one"`}},
		{name: "tag plus wildcard", values: []string{`"one", *`}},
		{name: "empty member", values: []string{`"one", , "two"`}},
		{name: "trailing comma", values: []string{`"one",`}},
		{name: "unterminated", values: []string{`"one`}},
		{name: "embedded space", values: []string{`"one two"`}},
		{name: "empty field", values: []string{" \t"}},
		{name: "no field values"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseEntityTagSet(test.values)
			if !test.valid {
				if err == nil {
					t.Fatalf("parseEntityTagSet(%q) succeeded: %#v", test.values, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEntityTagSet(%q): %v", test.values, err)
			}
			if got.Any != test.wantAny || !reflect.DeepEqual(got.Tags, test.want) {
				t.Fatalf("parseEntityTagSet(%q) = %#v, want any=%v tags=%#v", test.values, got, test.wantAny, test.want)
			}
		})
	}
}

func TestHTTPConditionEvaluation(t *testing.T) {
	t.Parallel()
	modified := time.Date(2026, time.August, 29, 10, 11, 12, 987, time.UTC)
	equalDate := modified.Truncate(time.Second).Format(http.TimeFormat)
	oldDate := modified.Add(-time.Hour).Format(http.TimeFormat)
	newDate := modified.Add(time.Hour).Format(http.TimeFormat)

	for _, test := range []struct {
		name        string
		method      string
		headers     map[string]string
		current     conditionState
		hasRange    bool
		wantOutcome conditionOutcome
		ignoreRange bool
	}{
		{name: "unconditional", method: http.MethodPut, current: conditionState{Exists: true, ETag: "v1"}, wantOutcome: conditionProceed},
		{name: "if match strong", method: http.MethodPut, headers: map[string]string{"If-Match": `"v1"`}, current: conditionState{Exists: true, ETag: "v1"}, wantOutcome: conditionProceed},
		{name: "if match weak never strongly matches", method: http.MethodPut, headers: map[string]string{"If-Match": `W/"v1"`}, current: conditionState{Exists: true, ETag: "v1"}, wantOutcome: conditionPreconditionFailed},
		{name: "if match list", method: http.MethodDelete, headers: map[string]string{"If-Match": `"old", "v1"`}, current: conditionState{Exists: true, ETag: `"v1"`}, wantOutcome: conditionProceed},
		{name: "if match wildcard existing", method: http.MethodPut, headers: map[string]string{"If-Match": "*"}, current: conditionState{Exists: true}, wantOutcome: conditionProceed},
		{name: "if match wildcard missing", method: http.MethodPut, headers: map[string]string{"If-Match": "*"}, current: conditionState{}, wantOutcome: conditionPreconditionFailed},
		{name: "if unmodified since stale", method: http.MethodPut, headers: map[string]string{"If-Unmodified-Since": oldDate}, current: conditionState{Exists: true, LastModified: modified}, wantOutcome: conditionPreconditionFailed},
		{name: "if match suppresses unmodified since", method: http.MethodPut, headers: map[string]string{"If-Match": `"v1"`, "If-Unmodified-Since": oldDate}, current: conditionState{Exists: true, ETag: "v1", LastModified: modified}, wantOutcome: conditionProceed},
		{name: "if none weak match get", method: http.MethodGet, headers: map[string]string{"If-None-Match": `W/"v1"`}, current: conditionState{Exists: true, ETag: "v1"}, wantOutcome: conditionNotModified},
		{name: "if none match put", method: http.MethodPut, headers: map[string]string{"If-None-Match": `"v1"`}, current: conditionState{Exists: true, ETag: "v1"}, wantOutcome: conditionPreconditionFailed},
		{name: "create only existing", method: http.MethodPut, headers: map[string]string{"If-None-Match": "*"}, current: conditionState{Exists: true}, wantOutcome: conditionPreconditionFailed},
		{name: "create only missing", method: http.MethodPut, headers: map[string]string{"If-None-Match": "*"}, current: conditionState{}, wantOutcome: conditionProceed},
		{name: "if modified since equal", method: http.MethodGet, headers: map[string]string{"If-Modified-Since": equalDate}, current: conditionState{Exists: true, LastModified: modified}, wantOutcome: conditionNotModified},
		{name: "if modified since older", method: http.MethodHead, headers: map[string]string{"If-Modified-Since": oldDate}, current: conditionState{Exists: true, LastModified: modified}, wantOutcome: conditionProceed},
		{name: "if none match suppresses modified since", method: http.MethodGet, headers: map[string]string{"If-None-Match": `"other"`, "If-Modified-Since": newDate}, current: conditionState{Exists: true, ETag: "v1", LastModified: modified}, wantOutcome: conditionProceed},
		{name: "dav if precedes cache validator", method: http.MethodGet, headers: map[string]string{"If": `(["stale"])`, "If-None-Match": `"v1"`}, current: conditionState{Exists: true, ETag: "v1"}, wantOutcome: conditionPreconditionFailed},
		{name: "matching if range", method: http.MethodGet, headers: map[string]string{"If-Range": `"v1"`}, current: conditionState{Exists: true, ETag: "v1"}, hasRange: true, wantOutcome: conditionProceed},
		{name: "stale if range", method: http.MethodGet, headers: map[string]string{"If-Range": `"old"`}, current: conditionState{Exists: true, ETag: "v1"}, hasRange: true, wantOutcome: conditionProceed, ignoreRange: true},
		{name: "weak if range", method: http.MethodGet, headers: map[string]string{"If-Range": `W/"v1"`}, current: conditionState{Exists: true, ETag: "v1"}, hasRange: true, wantOutcome: conditionProceed, ignoreRange: true},
		{name: "date if range is never a strong validator", method: http.MethodGet, headers: map[string]string{"If-Range": equalDate}, current: conditionState{Exists: true, LastModified: modified}, hasRange: true, wantOutcome: conditionProceed, ignoreRange: true},
		{name: "newer date if range is not exact", method: http.MethodGet, headers: map[string]string{"If-Range": newDate}, current: conditionState{Exists: true, LastModified: modified}, hasRange: true, wantOutcome: conditionProceed, ignoreRange: true},
		{name: "stale date if range", method: http.MethodGet, headers: map[string]string{"If-Range": oldDate}, current: conditionState{Exists: true, LastModified: modified}, hasRange: true, wantOutcome: conditionProceed, ignoreRange: true},
		{name: "if range ignored without range", method: http.MethodGet, headers: map[string]string{"If-Range": `"old"`}, current: conditionState{Exists: true, ETag: "v1"}, wantOutcome: conditionProceed},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(test.method, "https://example.com/file", nil)
			if test.hasRange {
				request.Header.Set("Range", "bytes=0-0")
			}
			for name, value := range test.headers {
				request.Header.Set(name, value)
			}
			conditions, err := parseRequestConditions(request)
			if err != nil {
				t.Fatalf("parseRequestConditions: %v", err)
			}
			got, err := conditions.evaluate(test.method, test.current, test.hasRange, nil)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if got.Outcome != test.wantOutcome || got.IgnoreRange != test.ignoreRange {
				t.Fatalf("evaluation = %#v, want outcome=%v ignoreRange=%v", got, test.wantOutcome, test.ignoreRange)
			}
		})
	}
}

func TestMalformedConditionalFieldsAreRejected(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		header http.Header
	}{
		{name: "empty if match", header: http.Header{"If-Match": {""}}},
		{name: "unquoted if match", header: http.Header{"If-Match": {"v1"}}},
		{name: "mixed wildcard", header: http.Header{"If-None-Match": {`*, "v1"`}}},
		{name: "trailing list comma", header: http.Header{"If-None-Match": {`"v1",`}}},
		{name: "empty dav if", header: http.Header{"If": {""}}},
		{name: "duplicate dav if", header: http.Header{"If": {`(["v1"])`, `(["v2"])`}}},
		{name: "empty dav list", header: http.Header{"If": {"()"}}},
		{name: "unterminated dav list", header: http.Header{"If": {`(["v1"]`}}},
		{name: "mixed dav forms", header: http.Header{"If": {`(["v1"]) </file> (["v1"])`}}},
		{name: "tag without list", header: http.Header{"If": {`</file>`}}},
		{name: "relative resource tag", header: http.Header{"If": {`<file> (["v1"])`}}},
		{name: "other authority", header: http.Header{"If": {`<https://other.example/file> (["v1"])`}}},
		{name: "resource tag query", header: http.Header{"If": {`</file?version=1> (["v1"])`}}},
		{name: "resource tag empty query", header: http.Header{"If": {`</file?> (["v1"])`}}},
		{name: "dot segment resource", header: http.Header{"If": {`</a/../file> (["v1"])`}}},
		{name: "relative state token", header: http.Header{"If": {`(<lock-token>)`}}},
		{name: "whitespace inside etag brackets", header: http.Header{"If": {`(["v1" ])`}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodPut, "https://example.com/file", nil)
			request.Header = test.header.Clone()
			_, err := parseRequestConditions(request)
			if !errors.Is(err, errInvalidConditionalHeader) {
				t.Fatalf("parseRequestConditions error = %v, want errInvalidConditionalHeader", err)
			}
		})
	}
}

func TestInvalidDateConditionsAreIgnored(t *testing.T) {
	t.Parallel()
	date := time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC).Format(http.TimeFormat)
	for _, test := range []struct {
		name        string
		method      string
		header      http.Header
		wantNoIUS   bool
		wantNoIMS   bool
		ignoreRange bool
	}{
		{name: "invalid if unmodified since", method: http.MethodPut, header: http.Header{"If-Unmodified-Since": {"yesterday-ish"}}, wantNoIUS: true},
		{name: "duplicate if modified since", method: http.MethodGet, header: http.Header{"If-Modified-Since": {date, date}}, wantNoIMS: true},
		{name: "if modified since ignored for put", method: http.MethodPut, header: http.Header{"If-Modified-Since": {date}}, wantNoIMS: true},
		{name: "invalid if range falls back to full response", method: http.MethodGet, header: http.Header{"Range": {"bytes=0-0"}, "If-Range": {"v1"}}, ignoreRange: true},
		{name: "duplicate if range falls back to full response", method: http.MethodGet, header: http.Header{"Range": {"bytes=0-0"}, "If-Range": {`"v1"`, `"v2"`}}, ignoreRange: true},
		{name: "if range ignored without range", method: http.MethodGet, header: http.Header{"If-Range": {"v1"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(test.method, "https://example.com/file", nil)
			request.Header = test.header.Clone()
			conditions, err := parseRequestConditions(request)
			if err != nil {
				t.Fatalf("parseRequestConditions: %v", err)
			}
			if test.wantNoIUS && conditions.ifUnmodifiedSince != nil {
				t.Fatalf("ifUnmodifiedSince = %v, want nil", conditions.ifUnmodifiedSince)
			}
			if test.wantNoIMS && conditions.ifModifiedSince != nil {
				t.Fatalf("ifModifiedSince = %v, want nil", conditions.ifModifiedSince)
			}
			got, err := conditions.evaluate(test.method, conditionState{Exists: true, ETag: "v1"}, request.Header.Get("Range") != "", nil)
			if err != nil || got.Outcome != conditionProceed || got.IgnoreRange != test.ignoreRange {
				t.Fatalf("evaluation = %#v, error = %v, want proceed ignoreRange=%v", got, err, test.ignoreRange)
			}
		})
	}
}

func TestOptionsIgnoresConditionalFields(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodOptions, "https://example.com/file", nil)
	request.Header.Set("If-Match", "not-an-etag")
	request.Header.Set("If", "not-a-dav-if-header")
	conditions, err := parseRequestConditions(request)
	if err != nil {
		t.Fatalf("parseRequestConditions: %v", err)
	}
	got, err := conditions.evaluate(request.Method, conditionState{}, false, nil)
	if err != nil || got.Outcome != conditionProceed {
		t.Fatalf("OPTIONS evaluation = %#v, error = %v", got, err)
	}
}

func TestDAVIfEvaluationAndSubmittedTokens(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPut, "https://example.com/a", nil)
	request.Header.Set("If", `(<opaquelocktoken:one> ["v1"]) (Not <DAV:no-lock>) (<opaquelocktoken:two>)`)
	conditions, err := parseRequestConditions(request)
	if err != nil {
		t.Fatalf("parseRequestConditions: %v", err)
	}
	if got, want := conditions.davIf.submittedLockTokens(), []string{"DAV:no-lock", "opaquelocktoken:one", "opaquelocktoken:two"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("submittedLockTokens = %v, want %v", got, want)
	}

	matching := conditionState{Exists: true, ETag: "v1", LockTokens: map[string]struct{}{"opaquelocktoken:one": {}}}
	applicable, satisfied := conditions.davIf.evaluateResource("/a", matching)
	if !applicable || !satisfied {
		t.Fatalf("matching resource evaluation = applicable %v, satisfied %v", applicable, satisfied)
	}

	// The first list is an AND and fails without the ETag, while the second
	// list demonstrates OR and Not by matching the reserved DAV:no-lock token.
	withoutETag := conditionState{Exists: true, ETag: "other", LockTokens: map[string]struct{}{"opaquelocktoken:one": {}}}
	applicable, satisfied = conditions.davIf.evaluateResource("/a", withoutETag)
	if !applicable || !satisfied {
		t.Fatalf("OR/Not evaluation = applicable %v, satisfied %v", applicable, satisfied)
	}

	got, err := conditions.evaluate(http.MethodPut, matching, false, nil)
	if err != nil || got.Outcome != conditionProceed {
		t.Fatalf("request evaluation = %#v, error = %v", got, err)
	}
}

func TestDAVIfTaggedResources(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest("MOVE", "https://example.com/source", nil)
	request.Header.Set("If", `</source> (["source-v1"]) <https://example.com/destination> (["stale"]) (<opaquelocktoken:destination>)`)
	conditions, err := parseRequestConditions(request)
	if err != nil {
		t.Fatalf("parseRequestConditions: %v", err)
	}
	if len(conditions.davIf.lists) != 3 || conditions.davIf.lists[0].resource != "/source" || conditions.davIf.lists[1].resource != "/destination" || conditions.davIf.lists[2].resource != "/destination" {
		t.Fatalf("parsed tagged lists = %#v", conditions.davIf.lists)
	}

	resolved := make(map[string]int)
	resolver := func(resource string) (conditionState, error) {
		resolved[resource]++
		switch resource {
		case "/destination":
			return conditionState{Exists: true, LockTokens: map[string]struct{}{"opaquelocktoken:destination": {}}}, nil
		default:
			return conditionState{}, nil
		}
	}
	got, err := conditions.evaluate("MOVE", conditionState{Exists: true, ETag: "source-v1"}, false, resolver)
	if err != nil || got.Outcome != conditionProceed {
		t.Fatalf("tagged evaluation = %#v, error = %v", got, err)
	}
	if resolved["/destination"] != 0 {
		t.Fatalf("destination resolutions = %d, want 0 after source production matched", resolved["/destination"])
	}

	applicable, satisfied := conditions.davIf.evaluateResource("/destination", conditionState{Exists: true, LockTokens: map[string]struct{}{"opaquelocktoken:destination": {}}})
	if !applicable || !satisfied {
		t.Fatalf("destination evaluation = applicable %v, satisfied %v", applicable, satisfied)
	}
	applicable, satisfied = conditions.davIf.evaluateResource("/unmentioned", conditionState{})
	if applicable || satisfied {
		t.Fatalf("unmentioned resource evaluation = applicable %v, satisfied %v", applicable, satisfied)
	}

	got, err = conditions.evaluate("MOVE", conditionState{Exists: true, ETag: "other"}, false, func(resource string) (conditionState, error) {
		return conditionState{Exists: true, LockTokens: map[string]struct{}{"opaquelocktoken:destination": {}}}, nil
	})
	if err != nil || got.Outcome != conditionProceed {
		t.Fatalf("matching destination production = %#v, error = %v", got, err)
	}

	got, err = conditions.evaluate("MOVE", conditionState{Exists: true, ETag: "other"}, false, func(resource string) (conditionState, error) {
		return conditionState{Exists: true, ETag: "other"}, nil
	})
	if err != nil || got.Outcome != conditionPreconditionFailed {
		t.Fatalf("all tagged productions failed = %#v, error = %v", got, err)
	}
}

func TestConditionResourcesPreserveLiteralPercentEscapes(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPut, "https://example.com/folder/%252e%252e/literal%252Fname", nil)
	request.Header.Set("If", `</folder/%252e%252e/literal%252Fname> (["v1"])`)
	conditions, err := parseRequestConditions(request)
	if err != nil {
		t.Fatalf("parseRequestConditions: %v", err)
	}
	const resource = "/folder/%252e%252e/literal%252Fname"
	if conditions.requestResource != resource || len(conditions.davIf.lists) != 1 || conditions.davIf.lists[0].resource != resource {
		t.Fatalf("canonical resources = request %q lists %#v", conditions.requestResource, conditions.davIf.lists)
	}
	key, err := conditionResourceKey(resource)
	if err != nil || key != "folder/%2e%2e/literal%2Fname" {
		t.Fatalf("conditionResourceKey = %q, %v", key, err)
	}
}

func TestDAVIfWeakETagUsesStrongComparison(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPut, "https://example.com/file", nil)
	request.Header.Set("If", `([W/"v1"])`)
	conditions, err := parseRequestConditions(request)
	if err != nil {
		t.Fatalf("parseRequestConditions: %v", err)
	}
	got, err := conditions.evaluate(http.MethodPut, conditionState{Exists: true, ETag: "v1"}, false, nil)
	if err != nil || got.Outcome != conditionPreconditionFailed {
		t.Fatalf("weak DAV ETag evaluation = %#v, error = %v", got, err)
	}
}

func TestDAVIfUnmappedResourceAndNot(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPut, "https://example.com/new", nil)
	request.Header.Set("If", `(Not ["old"])`)
	conditions, err := parseRequestConditions(request)
	if err != nil {
		t.Fatalf("parseRequestConditions: %v", err)
	}
	got, err := conditions.evaluate(http.MethodPut, conditionState{}, false, nil)
	if err != nil || got.Outcome != conditionProceed {
		t.Fatalf("unmapped Not evaluation = %#v, error = %v", got, err)
	}
}
