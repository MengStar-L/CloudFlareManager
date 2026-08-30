package webdavprotocol

import (
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

func TestHandlerGameSyncConditionalWriteProbe(t *testing.T) {
	t.Parallel()
	handler := conditionalTestHandler()

	first := performDAVRequest(handler, http.MethodPut, "/GameSync/catalog.json", "first", map[string]string{"If-None-Match": "*"})
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, body = %s", first.Code, first.Body.String())
	}
	firstETag := first.Header().Get("ETag")
	if !strings.HasPrefix(firstETag, `"`) || !strings.HasSuffix(firstETag, `"`) {
		t.Fatalf("first ETag = %q, want quoted strong validator", firstETag)
	}

	duplicate := performDAVRequest(handler, http.MethodPut, "/GameSync/catalog.json", "duplicate", map[string]string{"If-None-Match": "*"})
	if duplicate.Code != http.StatusPreconditionFailed {
		t.Fatalf("duplicate create status = %d, want 412", duplicate.Code)
	}
	stale := performDAVRequest(handler, http.MethodPut, "/GameSync/catalog.json", "stale", map[string]string{"If-Match": `"not-current"`})
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale update status = %d, want 412", stale.Code)
	}

	update := performDAVRequest(handler, http.MethodPut, "/GameSync/catalog.json", "second", map[string]string{"If-Match": firstETag})
	if update.Code != http.StatusNoContent {
		t.Fatalf("matching update status = %d, body = %s", update.Code, update.Body.String())
	}
	secondETag := update.Header().Get("ETag")
	if secondETag == "" || secondETag == firstETag {
		t.Fatalf("updated ETag = %q, first = %q", secondETag, firstETag)
	}
	oldVersion := performDAVRequest(handler, http.MethodPut, "/GameSync/catalog.json", "third", map[string]string{"If-Match": firstETag})
	if oldVersion.Code != http.StatusPreconditionFailed {
		t.Fatalf("old ETag update status = %d, want 412", oldVersion.Code)
	}

	get := performDAVRequest(handler, http.MethodGet, "/GameSync/catalog.json", "", nil)
	if get.Code != http.StatusOK || get.Body.String() != "second" || get.Header().Get("ETag") != secondETag {
		t.Fatalf("GET = %d %q ETag %q", get.Code, get.Body.String(), get.Header().Get("ETag"))
	}
	propfind := performDAVRequest(handler, "PROPFIND", "/GameSync/catalog.json", "", map[string]string{"Depth": "0"})
	if propfind.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND status = %d, body = %s", propfind.Code, propfind.Body.String())
	}
	var properties multistatus
	if err := xml.Unmarshal(propfind.Body.Bytes(), &properties); err != nil {
		t.Fatal(err)
	}
	if len(properties.Responses) != 1 || properties.Responses[0].PropStat.Properties.ETag != secondETag {
		t.Fatalf("PROPFIND ETag response = %#v, want %q", properties.Responses, secondETag)
	}
}

func TestHandlerReadConditionsAndStrictSyntax(t *testing.T) {
	t.Parallel()
	handler := conditionalTestHandler()
	created := performDAVRequest(handler, http.MethodPut, "/cache.txt", "cached", nil)
	etag := created.Header().Get("ETag")

	notModified := performDAVRequest(handler, http.MethodGet, "/cache.txt", "", map[string]string{"If-None-Match": "W/" + etag})
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 || notModified.Header().Get("ETag") != etag {
		t.Fatalf("If-None-Match GET = %d %q ETag %q", notModified.Code, notModified.Body.String(), notModified.Header().Get("ETag"))
	}
	failedHead := performDAVRequest(handler, http.MethodHead, "/cache.txt", "", map[string]string{"If-Match": `"other"`})
	if failedHead.Code != http.StatusPreconditionFailed {
		t.Fatalf("If-Match HEAD status = %d", failedHead.Code)
	}
	future := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	dateCached := performDAVRequest(handler, http.MethodGet, "/cache.txt", "", map[string]string{"If-Modified-Since": future})
	if dateCached.Code != http.StatusNotModified {
		t.Fatalf("If-Modified-Since GET status = %d", dateCached.Code)
	}
	malformed := performDAVRequest(handler, http.MethodGet, "/cache.txt", "", map[string]string{"If-Match": "unquoted"})
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed If-Match status = %d, want 400", malformed.Code)
	}
}

func TestHandlerReadUsesOneIndexedValidatorSnapshot(t *testing.T) {
	t.Parallel()
	objects := &recordingGetObjects{
		memoryObjects:        &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)},
		physicalLastModified: time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	handler := Handler{
		Objects: objects,
		Verify: func(context.Context, string, string) (Identity, error) {
			return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
		},
	}
	created := performDAVRequest(handler, http.MethodPut, "/snapshot.txt", "snapshot", nil)
	etag := created.Header().Get("ETag")
	indexed := objects.metadata[r2.WebDAVMountPrefix("credential")+"snapshot.txt"]

	get := performDAVRequest(handler, http.MethodGet, "/snapshot.txt", "", map[string]string{"If-Match": etag})
	if get.Code != http.StatusOK || get.Body.String() != "snapshot" {
		t.Fatalf("GET snapshot = %d %q", get.Code, get.Body.String())
	}
	if objects.lastOptions.IfMatch != etag {
		t.Fatalf("physical GET If-Match = %q, want %q", objects.lastOptions.IfMatch, etag)
	}
	if got, want := get.Header().Get("Last-Modified"), indexed.LastModified.UTC().Format(http.TimeFormat); got != want {
		t.Fatalf("Last-Modified = %q, want indexed value %q", got, want)
	}

	head := performDAVRequest(handler, http.MethodHead, "/snapshot.txt", "", map[string]string{"Range": "bytes=0-0"})
	if head.Code != http.StatusOK || objects.lastOptions.Range != "" {
		t.Fatalf("HEAD with Range = %d, physical range %q", head.Code, objects.lastOptions.Range)
	}
}

func TestHandlerReadReevaluatesSameETagLogicalReplacement(t *testing.T) {
	t.Parallel()
	modified := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		headers    map[string]string
		wantStatus int
		wantBody   string
		wantGets   int
	}{
		{
			name:       "date precondition is reevaluated",
			headers:    map[string]string{"If-Unmodified-Since": modified.Format(http.TimeFormat)},
			wantStatus: http.StatusPreconditionFailed, wantGets: 1,
		},
		{name: "unconditional request retries", wantStatus: http.StatusOK, wantBody: "second", wantGets: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := &sameETagReplacingObjects{
				memoryObjects: &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)},
				modified:      modified,
			}
			handler := Handler{
				Objects: objects,
				Verify: func(context.Context, string, string) (Identity, error) {
					return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
				},
			}
			response := performDAVRequest(handler, http.MethodGet, "/snapshot.txt", "", test.headers)
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody && test.wantBody != "" {
				t.Fatalf("response = %d %q, want %d %q", response.Code, response.Body.String(), test.wantStatus, test.wantBody)
			}
			if objects.getCalls != test.wantGets {
				t.Fatalf("GET calls = %d, want %d", objects.getCalls, test.wantGets)
			}
		})
	}
}

func TestHandlerConditionalCopyAndMove(t *testing.T) {
	t.Parallel()
	handler := conditionalTestHandler()
	source := performDAVRequest(handler, http.MethodPut, "/source.txt", "source", nil)
	sourceETag := source.Header().Get("ETag")

	stale := performDAVRequest(handler, "COPY", "/source.txt", "", map[string]string{
		"Destination": "/stale.txt", "If-Match": `"stale"`,
	})
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale COPY status = %d", stale.Code)
	}
	created := performDAVRequest(handler, "COPY", "/source.txt", "", map[string]string{
		"Destination": "/copy.txt", "Overwrite": "F", "If-Match": sourceETag,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("COPY create status = %d, body = %s", created.Code, created.Body.String())
	}
	conflict := performDAVRequest(handler, "COPY", "/source.txt", "", map[string]string{
		"Destination": "/copy.txt", "Overwrite": "F",
	})
	if conflict.Code != http.StatusPreconditionFailed {
		t.Fatalf("COPY Overwrite:F status = %d", conflict.Code)
	}
	overwrite := performDAVRequest(handler, "COPY", "/source.txt", "", map[string]string{"Destination": "/copy.txt"})
	if overwrite.Code != http.StatusNoContent {
		t.Fatalf("COPY overwrite status = %d", overwrite.Code)
	}

	moved := performDAVRequest(handler, "MOVE", "/source.txt", "", map[string]string{
		"Destination": "/moved.txt", "If-Match": sourceETag,
	})
	if moved.Code != http.StatusCreated {
		t.Fatalf("MOVE status = %d, body = %s", moved.Code, moved.Body.String())
	}
	missingSource := performDAVRequest(handler, http.MethodGet, "/source.txt", "", nil)
	if missingSource.Code != http.StatusNotFound {
		t.Fatalf("source after MOVE status = %d", missingSource.Code)
	}
	movedBody := performDAVRequest(handler, http.MethodGet, "/moved.txt", "", nil)
	if movedBody.Code != http.StatusOK || movedBody.Body.String() != "source" {
		t.Fatalf("moved object = %d %q", movedBody.Code, movedBody.Body.String())
	}
}

func TestHandlerFileCopyMoveRejectsCollectionDestination(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"COPY", "MOVE"} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			handler := conditionalTestHandler()
			performDAVRequest(handler, http.MethodPut, "/source.txt", "source", nil)
			performDAVRequest(handler, http.MethodPut, "/target/stale.txt", "stale", nil)

			response := performDAVRequest(handler, method, "/source.txt", "", map[string]string{"Destination": "/target/"})
			if response.Code != http.StatusConflict {
				t.Fatalf("%s file to collection destination status = %d, body = %s", method, response.Code, response.Body.String())
			}
			if source := performDAVRequest(handler, http.MethodGet, "/source.txt", "", nil); source.Code != http.StatusOK || source.Body.String() != "source" {
				t.Fatalf("source after rejected %s = %d %q", method, source.Code, source.Body.String())
			}
			if stale := performDAVRequest(handler, http.MethodGet, "/target/stale.txt", "", nil); stale.Code != http.StatusOK || stale.Body.String() != "stale" {
				t.Fatalf("destination after rejected %s = %d %q", method, stale.Code, stale.Body.String())
			}
		})
	}
}

func TestHandlerFailedSingleObjectOverwritePreservesDestination(t *testing.T) {
	t.Parallel()
	objects := &failingGetObjects{memoryObjects: &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}}
	handler := Handler{
		Objects: objects,
		Verify: func(context.Context, string, string) (Identity, error) {
			return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
		},
	}
	performDAVRequest(handler, http.MethodPut, "/source.txt", "source", nil)
	performDAVRequest(handler, http.MethodPut, "/destination.txt", "destination", nil)
	objects.failSuffix = "source.txt"

	overwrite := performDAVRequest(handler, "COPY", "/source.txt", "", map[string]string{"Destination": "/destination.txt"})
	if overwrite.Code != http.StatusConflict {
		t.Fatalf("failed overwrite status = %d, body = %s", overwrite.Code, overwrite.Body.String())
	}
	destination := performDAVRequest(handler, http.MethodGet, "/destination.txt", "", nil)
	if destination.Code != http.StatusOK || destination.Body.String() != "destination" {
		t.Fatalf("destination after failed overwrite = %d %q", destination.Code, destination.Body.String())
	}
}

func TestHandlerCopyValidatesDestinationAndTaggedCondition(t *testing.T) {
	t.Parallel()
	handler := conditionalTestHandler()
	performDAVRequest(handler, http.MethodPut, "/source.txt", "source", nil)
	destination := performDAVRequest(handler, http.MethodPut, "/destination.txt", "old", nil)
	destinationETag := destination.Header().Get("ETag")

	tagged := performDAVRequest(handler, "COPY", "/source.txt", "", map[string]string{
		"Destination": "http://example.com/destination.txt",
		"If":          "<http://example.com/destination.txt> ([" + destinationETag + "])",
	})
	if tagged.Code != http.StatusNoContent {
		t.Fatalf("tagged COPY status = %d, body = %s", tagged.Code, tagged.Body.String())
	}
	remote := performDAVRequest(handler, "COPY", "/source.txt", "", map[string]string{"Destination": "https://other.example/destination.txt"})
	if remote.Code != http.StatusBadGateway {
		t.Fatalf("remote Destination status = %d", remote.Code)
	}
	badOverwrite := performDAVRequest(handler, "COPY", "/source.txt", "", map[string]string{"Destination": "/other.txt", "Overwrite": "false"})
	if badOverwrite.Code != http.StatusBadRequest {
		t.Fatalf("invalid Overwrite status = %d", badOverwrite.Code)
	}
}

func TestHandlerLockTokensProtectMutationsAndRefresh(t *testing.T) {
	t.Parallel()
	locks, _ := newTestLockStore(t)
	handler := conditionalTestHandler()
	handler.Locks = locks
	performDAVRequest(handler, http.MethodPut, "/locked.txt", "initial", nil)
	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner>test</D:owner></D:lockinfo>`
	locked := performDAVRequest(handler, "LOCK", "/locked.txt", lockBody, map[string]string{
		"Depth": "0", "Timeout": "Second-120", "X-Forwarded-Proto": "https",
	})
	if locked.Code != http.StatusOK {
		t.Fatalf("LOCK status = %d, body = %s", locked.Code, locked.Body.String())
	}
	tokenHeader := locked.Header().Get("Lock-Token")
	token := strings.Trim(tokenHeader, "<>")
	if token == "" {
		t.Fatal("LOCK did not return a token")
	}
	var lockProperties lockResponse
	if err := xml.Unmarshal(locked.Body.Bytes(), &lockProperties); err != nil {
		t.Fatalf("decode LOCK response: %v", err)
	}
	if len(lockProperties.Discovery.ActiveLocks) != 1 {
		t.Fatalf("LOCK discovery = %#v", lockProperties.Discovery)
	}
	active := lockProperties.Discovery.ActiveLocks[0]
	if active.Scope.Exclusive == nil || active.Type.Write == nil || active.Depth != "0" || active.Token.Href != token ||
		active.Root.Href != "https://example.com/locked.txt" || !strings.HasPrefix(active.Timeout, "Second-") ||
		active.Owner == nil || !strings.Contains(active.Owner.InnerXML, "test") {
		t.Fatalf("LOCK active lock = %#v", active)
	}
	propfind := performDAVRequest(handler, "PROPFIND", "/locked.txt", "", map[string]string{
		"Depth": "0", "X-Forwarded-Proto": "https",
	})
	if propfind.Code != http.StatusMultiStatus {
		t.Fatalf("locked PROPFIND status = %d, body = %s", propfind.Code, propfind.Body.String())
	}
	var multistatusProperties multistatus
	if err := xml.Unmarshal(propfind.Body.Bytes(), &multistatusProperties); err != nil {
		t.Fatalf("decode locked PROPFIND: %v", err)
	}
	properties := multistatusProperties.Responses[0].PropStat.Properties
	if properties.SupportedLock == nil || len(properties.SupportedLock.Entries) != 1 ||
		properties.SupportedLock.Entries[0].Scope.Exclusive == nil || properties.SupportedLock.Entries[0].Type.Write == nil {
		t.Fatalf("supportedlock = %#v", properties.SupportedLock)
	}
	if properties.LockDiscovery == nil || len(properties.LockDiscovery.ActiveLocks) != 1 ||
		properties.LockDiscovery.ActiveLocks[0].Token.Href != token ||
		properties.LockDiscovery.ActiveLocks[0].Root.Href != "https://example.com/locked.txt" {
		t.Fatalf("lockdiscovery = %#v", properties.LockDiscovery)
	}

	blocked := performDAVRequest(handler, http.MethodPut, "/locked.txt", "blocked", nil)
	if blocked.Code != http.StatusLocked {
		t.Fatalf("PUT without lock token status = %d", blocked.Code)
	}
	authorized := performDAVRequest(handler, http.MethodPut, "/locked.txt", "authorized", map[string]string{"If": "(<" + token + ">)"})
	if authorized.Code != http.StatusNoContent {
		t.Fatalf("PUT with lock token status = %d, body = %s", authorized.Code, authorized.Body.String())
	}
	refresh := performDAVRequest(handler, "LOCK", "/locked.txt", "", map[string]string{"If": "(<" + token + ">)", "Timeout": "Second-300"})
	if refresh.Code != http.StatusOK || refresh.Header().Get("Lock-Token") != "" {
		t.Fatalf("LOCK refresh = %d token %q", refresh.Code, refresh.Header().Get("Lock-Token"))
	}
	wrongURI := performDAVRequest(handler, "UNLOCK", "/other.txt", "", map[string]string{"Lock-Token": tokenHeader})
	if wrongURI.Code != http.StatusConflict {
		t.Fatalf("UNLOCK wrong URI status = %d", wrongURI.Code)
	}
	missingToken := performDAVRequest(handler, "UNLOCK", "/locked.txt", "", nil)
	if missingToken.Code != http.StatusBadRequest {
		t.Fatalf("UNLOCK missing token status = %d", missingToken.Code)
	}
	unlocked := performDAVRequest(handler, "UNLOCK", "/locked.txt", "", map[string]string{"Lock-Token": tokenHeader})
	if unlocked.Code != http.StatusNoContent {
		t.Fatalf("UNLOCK status = %d", unlocked.Code)
	}
}

func TestHandlerUnlockAllowsResourceWithinInfiniteLock(t *testing.T) {
	t.Parallel()
	locks, _ := newTestLockStore(t)
	handler := conditionalTestHandler()
	handler.Locks = locks
	if created := performDAVRequest(handler, "MKCOL", "/tree/", "", nil); created.Code != http.StatusCreated {
		t.Fatalf("MKCOL status = %d", created.Code)
	}
	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	locked := performDAVRequest(handler, "LOCK", "/tree/", lockBody, map[string]string{"Depth": "infinity"})
	if locked.Code != http.StatusOK {
		t.Fatalf("LOCK status = %d", locked.Code)
	}
	tokenHeader := locked.Header().Get("Lock-Token")
	childUnlock := performDAVRequest(handler, "UNLOCK", "/tree/child.txt", "", map[string]string{"Lock-Token": tokenHeader})
	if childUnlock.Code != http.StatusNoContent {
		t.Fatalf("UNLOCK descendant status = %d", childUnlock.Code)
	}
	rootUnlock := performDAVRequest(handler, "UNLOCK", "/tree/", "", map[string]string{"Lock-Token": tokenHeader})
	if rootUnlock.Code != http.StatusConflict {
		t.Fatalf("UNLOCK consumed token status = %d", rootUnlock.Code)
	}
}

func TestHandlerRefreshUsesOriginalLockRootAndOwner(t *testing.T) {
	t.Parallel()
	locks, _ := newTestLockStore(t)
	handler := conditionalTestHandler()
	handler.Locks = locks
	if created := performDAVRequest(handler, "MKCOL", "/tree/", "", nil); created.Code != http.StatusCreated {
		t.Fatalf("MKCOL status = %d", created.Code)
	}
	if created := performDAVRequest(handler, http.MethodPut, "/tree/child.txt", "child", nil); created.Code != http.StatusCreated {
		t.Fatalf("child PUT status = %d", created.Code)
	}
	lockBody := `<D:lockinfo xmlns:D="DAV:" xmlns:C="urn:contact"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner>Team <C:contact><C:href>mailto:sync@example.com</C:href></C:contact></D:owner></D:lockinfo>`
	locked := performDAVRequest(handler, "LOCK", "/tree/", lockBody, map[string]string{"Depth": "infinity", "X-Forwarded-Proto": "https"})
	if locked.Code != http.StatusOK {
		t.Fatalf("LOCK status = %d, body = %s", locked.Code, locked.Body.String())
	}
	token := strings.Trim(locked.Header().Get("Lock-Token"), "<>")
	refresh := performDAVRequest(handler, "LOCK", "/tree/child.txt", "", map[string]string{
		"If": "(<" + token + ">)", "Timeout": "Second-300", "X-Forwarded-Proto": "https",
	})
	if refresh.Code != http.StatusOK {
		t.Fatalf("descendant refresh status = %d, body = %s", refresh.Code, refresh.Body.String())
	}
	var response lockResponse
	if err := xml.Unmarshal(refresh.Body.Bytes(), &response); err != nil || len(response.Discovery.ActiveLocks) != 1 {
		t.Fatalf("decode refresh response: %v, %#v", err, response)
	}
	active := response.Discovery.ActiveLocks[0]
	if active.Root.Href != "https://example.com/tree/" || active.Owner == nil ||
		!strings.Contains(active.Owner.InnerXML, "Team") || !strings.Contains(active.Owner.InnerXML, "mailto:sync@example.com") ||
		!strings.Contains(active.Owner.InnerXML, "urn:contact") {
		t.Fatalf("refreshed active lock = %#v, XML = %s", active, refresh.Body.String())
	}
	propfind := performDAVRequest(handler, "PROPFIND", "/tree/child.txt", "", map[string]string{"Depth": "0", "X-Forwarded-Proto": "https"})
	var properties multistatus
	if propfind.Code != http.StatusMultiStatus || xml.Unmarshal(propfind.Body.Bytes(), &properties) != nil ||
		len(properties.Responses) != 1 || properties.Responses[0].PropStat.Properties.LockDiscovery == nil ||
		len(properties.Responses[0].PropStat.Properties.LockDiscovery.ActiveLocks) != 1 {
		t.Fatalf("locked child PROPFIND = %d body=%s", propfind.Code, propfind.Body.String())
	}
	discovered := properties.Responses[0].PropStat.Properties.LockDiscovery.ActiveLocks[0]
	if discovered.Root.Href != "https://example.com/tree/" || discovered.Owner == nil ||
		!strings.Contains(discovered.Owner.InnerXML, "mailto:sync@example.com") {
		t.Fatalf("discovered active lock = %#v", discovered)
	}
}

func TestHandlerLockPreservesOwnerElementSemantics(t *testing.T) {
	t.Parallel()
	locks, _ := newTestLockStore(t)
	handler := conditionalTestHandler()
	handler.Locks = locks
	if created := performDAVRequest(handler, http.MethodPut, "/owner.txt", "owner", nil); created.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d", created.Code)
	}
	lockBody := `<D:lockinfo xmlns:D="DAV:" xmlns:C="urn:contact"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner xml:lang="en-US" C:role="sync" data-mode="full">  Team <C:contact><C:href>mailto:sync@example.com</C:href></C:contact> tail  </D:owner></D:lockinfo>`
	locked := performDAVRequest(handler, "LOCK", "/owner.txt", lockBody, map[string]string{"Depth": "0"})
	if locked.Code != http.StatusOK {
		t.Fatalf("LOCK status = %d, body = %s", locked.Code, locked.Body.String())
	}
	token := strings.Trim(locked.Header().Get("Lock-Token"), "<>")
	persisted, err := locks.Get(context.Background(), token)
	if err != nil {
		t.Fatalf("read persisted lock: %v", err)
	}
	decoder := xml.NewDecoder(strings.NewReader(persisted.Owner))
	first, err := decoder.Token()
	start, ok := first.(xml.StartElement)
	if err != nil || !ok || start.Name.Space != "DAV:" || start.Name.Local != "owner" {
		t.Fatalf("persisted owner is not a complete DAV:owner element: %q (%v)", persisted.Owner, err)
	}
	persistedOwner := makeLockOwner(persisted.Owner)
	requireOwnerAttribute(t, persistedOwner, "http://www.w3.org/XML/1998/namespace", "lang", "en-US")
	requireOwnerAttribute(t, persistedOwner, "urn:contact", "role", "sync")
	requireOwnerAttribute(t, persistedOwner, "", "data-mode", "full")
	if !strings.HasPrefix(persistedOwner.InnerXML, "  Team ") || !strings.HasSuffix(persistedOwner.InnerXML, " tail  ") ||
		!strings.Contains(persistedOwner.InnerXML, "mailto:sync@example.com") {
		t.Fatalf("persisted owner contents = %q", persistedOwner.InnerXML)
	}

	refresh := performDAVRequest(handler, "LOCK", "/owner.txt", "", map[string]string{
		"If": "(<" + token + ">)", "Timeout": "Second-300",
	})
	if refresh.Code != http.StatusOK {
		t.Fatalf("LOCK refresh status = %d, body = %s", refresh.Code, refresh.Body.String())
	}
	var response lockResponse
	if err := xml.Unmarshal(refresh.Body.Bytes(), &response); err != nil || len(response.Discovery.ActiveLocks) != 1 {
		t.Fatalf("decode LOCK refresh: %v, %#v", err, response)
	}
	owner := response.Discovery.ActiveLocks[0].Owner
	requireOwnerAttribute(t, owner, "http://www.w3.org/XML/1998/namespace", "lang", "en-US")
	requireOwnerAttribute(t, owner, "urn:contact", "role", "sync")
	requireOwnerAttribute(t, owner, "", "data-mode", "full")
	if !strings.HasPrefix(owner.InnerXML, "  Team ") || !strings.HasSuffix(owner.InnerXML, " tail  ") ||
		!strings.Contains(owner.InnerXML, "mailto:sync@example.com") {
		t.Fatalf("refreshed owner contents = %q, XML = %s", owner.InnerXML, refresh.Body.String())
	}
}

func TestMakeLockOwnerSupportsLegacyStoredValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		stored     string
		want       string
		wantLang   string
		wantPrefix string
		wantSuffix string
	}{
		{
			name: "lockinfo document",
			stored: `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype>` +
				`<D:owner xml:lang="zh-CN">  legacy lockinfo  </D:owner></D:lockinfo>`,
			want: "legacy lockinfo", wantLang: "zh-CN", wantPrefix: "  ", wantSuffix: "  ",
		},
		{
			name: "owner element", stored: `<D:owner xmlns:D="DAV:" xml:lang="en"> old owner </D:owner>`,
			want: "old owner", wantLang: "en", wantPrefix: " ", wantSuffix: " ",
		},
		{name: "inner XML fragment", stored: `<C:href xmlns:C="urn:contact">mailto:legacy@example.com</C:href>`, want: "mailto:legacy@example.com"},
		{name: "plain text", stored: ` Team & Ops `, want: `Team &amp; Ops`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			owner := makeLockOwner(test.stored)
			if owner == nil || !strings.Contains(owner.InnerXML, test.want) ||
				!strings.HasPrefix(owner.InnerXML, test.wantPrefix) || !strings.HasSuffix(owner.InnerXML, test.wantSuffix) {
				t.Fatalf("makeLockOwner(%q) = %#v", test.stored, owner)
			}
			if test.wantLang != "" {
				requireOwnerAttribute(t, owner, "http://www.w3.org/XML/1998/namespace", "lang", test.wantLang)
			}
		})
	}
}

func requireOwnerAttribute(t *testing.T, owner *lockOwner, space, local, want string) {
	t.Helper()
	if owner == nil {
		t.Fatalf("owner is nil; want %s=%q", local, want)
	}
	for _, attribute := range owner.Attrs {
		if attribute.Name.Space == space && attribute.Name.Local == local {
			if attribute.Value != want {
				t.Fatalf("owner attribute {%s}%s = %q, want %q", space, local, attribute.Value, want)
			}
			return
		}
	}
	t.Fatalf("owner attribute {%s}%s missing from %#v", space, local, owner.Attrs)
}

func TestHandlerLockValidatesBodyDepthAndCreationStatus(t *testing.T) {
	t.Parallel()
	locks, _ := newTestLockStore(t)
	handler := conditionalTestHandler()
	handler.Locks = locks
	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	created := performDAVRequest(handler, "LOCK", "/new.txt", lockBody, map[string]string{"Depth": "0"})
	if created.Code != http.StatusCreated {
		t.Fatalf("LOCK unmapped resource status = %d", created.Code)
	}
	get := performDAVRequest(handler, http.MethodGet, "/new.txt", "", nil)
	if get.Code != http.StatusOK || get.Body.Len() != 0 || get.Header().Get("Content-Length") != "0" || get.Header().Get("ETag") == "" {
		t.Fatalf("GET lock-created resource = %d body=%q length=%q ETag=%q", get.Code, get.Body.String(), get.Header().Get("Content-Length"), get.Header().Get("ETag"))
	}
	propfind := performDAVRequest(handler, "PROPFIND", "/new.txt", "", map[string]string{"Depth": "0"})
	var properties multistatus
	if propfind.Code != http.StatusMultiStatus || xml.Unmarshal(propfind.Body.Bytes(), &properties) != nil ||
		len(properties.Responses) != 1 || properties.Responses[0].PropStat.Properties.ContentLength == nil ||
		*properties.Responses[0].PropStat.Properties.ContentLength != 0 {
		t.Fatalf("PROPFIND lock-created 0B resource = %d body=%s decoded=%#v", propfind.Code, propfind.Body.String(), properties)
	}
	badDepth := performDAVRequest(handler, "LOCK", "/depth.txt", lockBody, map[string]string{"Depth": "1"})
	if badDepth.Code != http.StatusBadRequest {
		t.Fatalf("LOCK bad Depth status = %d", badDepth.Code)
	}
	shared := strings.Replace(lockBody, "exclusive", "shared", 1)
	badScope := performDAVRequest(handler, "LOCK", "/shared.txt", shared, map[string]string{"Depth": "0"})
	if badScope.Code != http.StatusBadRequest {
		t.Fatalf("LOCK shared scope status = %d", badScope.Code)
	}
	evilNamespace := strings.ReplaceAll(lockBody, `xmlns:D="DAV:"`, `xmlns:D="urn:not-dav"`)
	if response := performDAVRequest(handler, "LOCK", "/evil.txt", evilNamespace, map[string]string{"Depth": "0"}); response.Code != http.StatusBadRequest {
		t.Fatalf("LOCK foreign namespace status = %d", response.Code)
	}
	duplicateScope := strings.Replace(lockBody, `</D:lockscope>`, `</D:lockscope><D:lockscope><D:exclusive/></D:lockscope>`, 1)
	if response := performDAVRequest(handler, "LOCK", "/duplicate.txt", duplicateScope, map[string]string{"Depth": "0"}); response.Code != http.StatusBadRequest {
		t.Fatalf("LOCK duplicate scope status = %d", response.Code)
	}
	trailingDocument := lockBody + lockBody
	if response := performDAVRequest(handler, "LOCK", "/trailing.txt", trailingDocument, map[string]string{"Depth": "0"}); response.Code != http.StatusBadRequest {
		t.Fatalf("LOCK trailing document status = %d", response.Code)
	}
	if response := performDAVRequest(handler, "LOCK", "/missing-collection/", lockBody, map[string]string{"Depth": "0"}); response.Code != http.StatusConflict {
		t.Fatalf("LOCK unmapped collection status = %d", response.Code)
	}
}

func TestHandlerLockRechecksResourceStateAfterCreatingLock(t *testing.T) {
	t.Parallel()
	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	for _, test := range []struct {
		name        string
		firstExists bool
		actualBody  string
		wantStatus  int
		wantPuts    int
	}{
		{name: "concurrent put won", actualBody: "existing", wantStatus: http.StatusOK},
		{name: "concurrent delete won", firstExists: true, wantStatus: http.StatusCreated, wantPuts: 1},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			locks, _ := newTestLockStore(t)
			base := &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}
			mountKey := r2.WebDAVMountPrefix("credential") + "race.txt"
			if test.actualBody != "" {
				if _, err := base.PutConditional(context.Background(), r2.PutRequest{
					Key: mountKey, Body: strings.NewReader(test.actualBody), Size: int64(len(test.actualBody)),
				}); err != nil {
					t.Fatal(err)
				}
			}
			objects := &firstStatOverrideObjects{memoryObjects: base, firstExists: test.firstExists}
			handler := Handler{
				Objects: objects, Locks: locks,
				Verify: func(context.Context, string, string) (Identity, error) {
					return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
				},
			}

			response := performDAVRequest(handler, "LOCK", "/race.txt", lockBody, map[string]string{"Depth": "0"})
			if response.Code != test.wantStatus || objects.putCalls != test.wantPuts {
				t.Fatalf("LOCK race = %d puts=%d body=%s", response.Code, objects.putCalls, response.Body.String())
			}
			get := performDAVRequest(handler, http.MethodGet, "/race.txt", "", nil)
			if get.Code != http.StatusOK {
				t.Fatalf("lock-root GET status = %d", get.Code)
			}
			if test.actualBody != "" && get.Body.String() != test.actualBody {
				t.Fatalf("existing body replaced = %q", get.Body.String())
			}
			if test.actualBody == "" && get.Body.Len() != 0 {
				t.Fatalf("lock-null body = %q", get.Body.String())
			}
		})
	}
}

func TestHandlerMutationGuardRechecksSubmittedLockToken(t *testing.T) {
	t.Parallel()
	locks, _ := newTestLockStore(t)
	handler := conditionalTestHandler()
	handler.Locks = locks
	if _, err := handler.Objects.PutConditional(context.Background(), r2.PutRequest{
		Key: "item.txt", Body: strings.NewReader("item"), Size: 4,
	}); err != nil {
		t.Fatal(err)
	}
	lock := createTestLock(t, locks, "item.txt", "0")
	request := httptest.NewRequest(http.MethodPut, "/item.txt", strings.NewReader("updated"))
	request.Header.Set("If", "(<"+lock.Token+">)")
	prepared, err := handler.prepareConditions(request, "item.txt")
	if err != nil || prepared.evaluation.Outcome != conditionProceed {
		t.Fatalf("prepare lock condition = %#v, %v", prepared.evaluation, err)
	}
	if err := locks.Delete(context.Background(), lock.Token); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	guard, stopped := handler.beginMutation(response, request, []string{"item.txt"}, &prepared)
	if guard != nil {
		guard.Release()
	}
	if !stopped || response.Code != http.StatusPreconditionFailed {
		t.Fatalf("mutation after token removal stopped=%v status=%d", stopped, response.Code)
	}
	if _, err := locks.Create(context.Background(), "item.txt", "owner", "0", time.Minute); err != nil {
		t.Fatalf("guard leaked mutex after precondition failure: %v", err)
	}
}

func TestHandlerCollectionLockProtectsMembership(t *testing.T) {
	t.Parallel()
	locks, _ := newTestLockStore(t)
	handler := conditionalTestHandler()
	handler.Locks = locks
	if created := performDAVRequest(handler, "MKCOL", "/docs/", "", nil); created.Code != http.StatusCreated {
		t.Fatalf("MKCOL status = %d", created.Code)
	}
	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	locked := performDAVRequest(handler, "LOCK", "/docs/", lockBody, map[string]string{"Depth": "0"})
	if locked.Code != http.StatusOK {
		t.Fatalf("LOCK collection status = %d", locked.Code)
	}
	token := strings.Trim(locked.Header().Get("Lock-Token"), "<>")
	blocked := performDAVRequest(handler, http.MethodPut, "/docs/new.txt", "blocked", nil)
	if blocked.Code != http.StatusLocked {
		t.Fatalf("PUT under depth-zero collection lock status = %d", blocked.Code)
	}
	authorized := performDAVRequest(handler, http.MethodPut, "/docs/new.txt", "allowed", map[string]string{
		"If": `</docs/> (<` + token + `>)`,
	})
	if authorized.Code != http.StatusCreated {
		t.Fatalf("PUT with tagged parent token status = %d, body = %s", authorized.Code, authorized.Body.String())
	}
}

func TestHandlerDepthZeroLockDoesNotFreezeExistingDescendants(t *testing.T) {
	t.Parallel()
	locks, _ := newTestLockStore(t)
	handler := conditionalTestHandler()
	handler.Locks = locks
	if created := performDAVRequest(handler, "MKCOL", "/a/", "", nil); created.Code != http.StatusCreated {
		t.Fatalf("MKCOL /a/ status = %d", created.Code)
	}
	if created := performDAVRequest(handler, "MKCOL", "/a/b/", "", nil); created.Code != http.StatusCreated {
		t.Fatalf("MKCOL /a/b/ status = %d", created.Code)
	}
	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	if locked := performDAVRequest(handler, "LOCK", "/a/", lockBody, map[string]string{"Depth": "0"}); locked.Code != http.StatusOK {
		t.Fatalf("LOCK /a/ status = %d", locked.Code)
	}
	created := performDAVRequest(handler, http.MethodPut, "/a/b/file.txt", "data", nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("nested PUT under depth-zero ancestor lock status = %d, body = %s", created.Code, created.Body.String())
	}
}

func TestHandlerVirtualCollectionLockNormalizesSlashAlias(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"PUT", "MKCOL", "DELETE"} {
		operation := operation
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			locks, _ := newTestLockStore(t)
			handler := conditionalTestHandler()
			handler.Locks = locks
			if created := performDAVRequest(handler, http.MethodPut, "/virtual/child.txt", "child", nil); created.Code != http.StatusCreated {
				t.Fatalf("seed child status = %d", created.Code)
			}
			lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
			locked := performDAVRequest(handler, "LOCK", "/virtual", lockBody, map[string]string{"Depth": "0"})
			if locked.Code != http.StatusOK {
				t.Fatalf("LOCK slash alias status = %d, body = %s", locked.Code, locked.Body.String())
			}
			var response *httptest.ResponseRecorder
			switch operation {
			case "PUT":
				response = performDAVRequest(handler, http.MethodPut, "/virtual/new.txt", "new", nil)
			case "MKCOL":
				response = performDAVRequest(handler, "MKCOL", "/virtual/new/", "", nil)
			case "DELETE":
				response = performDAVRequest(handler, http.MethodDelete, "/virtual/child.txt", "", nil)
			}
			if response.Code != http.StatusLocked {
				t.Fatalf("%s through slash alias lock status = %d, body = %s", operation, response.Code, response.Body.String())
			}
		})
	}
}

func TestHandlerCollectionDepthAndOverwriteSemantics(t *testing.T) {
	t.Parallel()
	handler := conditionalTestHandler()
	for target, body := range map[string]string{
		"/source/one.txt":        "one",
		"/source/nested/two.txt": "two",
		"/target/stale.txt":      "stale",
	} {
		if response := performDAVRequest(handler, http.MethodPut, target, body, nil); response.Code != http.StatusCreated {
			t.Fatalf("seed PUT %s status = %d", target, response.Code)
		}
	}

	shallow := performDAVRequest(handler, "COPY", "/source/", "", map[string]string{
		"Destination": "/shallow/", "Depth": "0",
	})
	if shallow.Code != http.StatusCreated {
		t.Fatalf("Depth:0 COPY status = %d, body = %s", shallow.Code, shallow.Body.String())
	}
	if child := performDAVRequest(handler, http.MethodGet, "/shallow/one.txt", "", nil); child.Code != http.StatusNotFound {
		t.Fatalf("Depth:0 COPY child status = %d", child.Code)
	}
	if collection := performDAVRequest(handler, "PROPFIND", "/shallow/", "", map[string]string{"Depth": "0"}); collection.Code != http.StatusMultiStatus {
		t.Fatalf("Depth:0 COPY collection PROPFIND status = %d", collection.Code)
	}

	overwrite := performDAVRequest(handler, "COPY", "/source/", "", map[string]string{"Destination": "/target/"})
	if overwrite.Code != http.StatusNoContent {
		t.Fatalf("collection overwrite COPY status = %d, body = %s", overwrite.Code, overwrite.Body.String())
	}
	if stale := performDAVRequest(handler, http.MethodGet, "/target/stale.txt", "", nil); stale.Code != http.StatusNotFound {
		t.Fatalf("overwritten destination-only child status = %d", stale.Code)
	}
	if copied := performDAVRequest(handler, http.MethodGet, "/target/nested/two.txt", "", nil); copied.Code != http.StatusOK || copied.Body.String() != "two" {
		t.Fatalf("recursive COPY child = %d %q", copied.Code, copied.Body.String())
	}

	for _, test := range []struct {
		method      string
		target      string
		destination string
		depth       string
	}{
		{method: "COPY", target: "/source/", destination: "/bad-copy/", depth: "1"},
		{method: "MOVE", target: "/source/", destination: "/bad-move/", depth: "0"},
		{method: http.MethodDelete, target: "/source/", depth: "0"},
	} {
		headers := map[string]string{"Depth": test.depth}
		if test.destination != "" {
			headers["Destination"] = test.destination
		}
		response := performDAVRequest(handler, test.method, test.target, "", headers)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s collection Depth:%s status = %d", test.method, test.depth, response.Code)
		}
	}
}

func TestHandlerExplicitCollectionSlashAliasKeepsValidator(t *testing.T) {
	t.Parallel()
	handler := conditionalTestHandler()
	if created := performDAVRequest(handler, "MKCOL", "/collection/", "", nil); created.Code != http.StatusCreated {
		t.Fatalf("MKCOL status = %d", created.Code)
	}
	readETag := func(target string) string {
		t.Helper()
		response := performDAVRequest(handler, "PROPFIND", target, "", map[string]string{"Depth": "0"})
		var properties multistatus
		if response.Code != http.StatusMultiStatus || xml.Unmarshal(response.Body.Bytes(), &properties) != nil || len(properties.Responses) != 1 {
			t.Fatalf("PROPFIND %s = %d body=%s", target, response.Code, response.Body.String())
		}
		return properties.Responses[0].PropStat.Properties.ETag
	}
	slashETag := readETag("/collection/")
	aliasETag := readETag("/collection")
	if slashETag == "" || aliasETag != slashETag {
		t.Fatalf("collection alias ETags: slash=%q alias=%q", slashETag, aliasETag)
	}
	created := performDAVRequest(handler, http.MethodPut, "/target.txt", "target", map[string]string{
		"If": "</collection> ([" + aliasETag + "])",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("tagged slashless collection validator status = %d, body = %s", created.Code, created.Body.String())
	}
}

func TestHandlerMKCOLRejectsUnknownLengthBody(t *testing.T) {
	t.Parallel()
	handler := conditionalTestHandler()
	request := httptest.NewRequest("MKCOL", "/chunked/", strings.NewReader("body"))
	request.SetBasicAuth("dav", "secret")
	request.ContentLength = -1
	request.TransferEncoding = []string{"chunked"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("chunked MKCOL body status = %d", response.Code)
	}
}

func TestHandlerPreservesAlreadyDecodedPercentSequences(t *testing.T) {
	t.Parallel()
	handler := conditionalTestHandler()
	if created := performDAVRequest(handler, http.MethodPut, "/victim.txt", "safe", nil); created.Code != http.StatusCreated {
		t.Fatalf("seed victim status = %d", created.Code)
	}
	encodedTarget := "/folder/%252e%252e/victim.txt"
	if created := performDAVRequest(handler, http.MethodPut, encodedTarget, "encoded", nil); created.Code != http.StatusCreated {
		t.Fatalf("encoded PUT status = %d", created.Code)
	}
	if encoded := performDAVRequest(handler, http.MethodGet, encodedTarget, "", nil); encoded.Code != http.StatusOK || encoded.Body.String() != "encoded" {
		t.Fatalf("encoded GET = %d %q", encoded.Code, encoded.Body.String())
	}
	if deleted := performDAVRequest(handler, http.MethodDelete, encodedTarget, "", nil); deleted.Code != http.StatusNoContent {
		t.Fatalf("encoded DELETE status = %d", deleted.Code)
	}
	if victim := performDAVRequest(handler, http.MethodGet, "/victim.txt", "", nil); victim.Code != http.StatusOK || victim.Body.String() != "safe" {
		t.Fatalf("victim after encoded DELETE = %d %q", victim.Code, victim.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/literal%252Fname", nil)
	key, err := requestKey(request.URL.Path)
	if err != nil || key != "literal%2Fname" {
		t.Fatalf("literal encoded slash key = %q, %v", key, err)
	}
}

func TestHandlerLockedMutationsPreservePercentEncodedNames(t *testing.T) {
	t.Parallel()
	locks, _ := newTestLockStore(t)
	handler := conditionalTestHandler()
	handler.Locks = locks
	if created := performDAVRequest(handler, http.MethodPut, "/victim.txt", "safe", nil); created.Code != http.StatusCreated {
		t.Fatalf("seed victim status = %d", created.Code)
	}
	target := "/folder/%252e%252e/literal%252Fname"
	created := performDAVRequest(handler, http.MethodPut, target, "encoded", nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("encoded PUT status = %d, body = %s", created.Code, created.Body.String())
	}
	etag := created.Header().Get("ETag")
	updated := performDAVRequest(handler, http.MethodPut, target, "updated", map[string]string{
		"If": "<" + target + "> ([" + etag + "])",
	})
	if updated.Code != http.StatusNoContent {
		t.Fatalf("tagged encoded PUT status = %d, body = %s", updated.Code, updated.Body.String())
	}
	propfind := performDAVRequest(handler, "PROPFIND", target, "", map[string]string{"Depth": "0"})
	var properties multistatus
	if propfind.Code != http.StatusMultiStatus || xml.Unmarshal(propfind.Body.Bytes(), &properties) != nil || len(properties.Responses) != 1 {
		t.Fatalf("encoded PROPFIND = %d body=%s", propfind.Code, propfind.Body.String())
	}
	if properties.Responses[0].Href != target {
		t.Fatalf("encoded href = %q, want %q", properties.Responses[0].Href, target)
	}
	if roundTrip := performDAVRequest(handler, http.MethodGet, properties.Responses[0].Href, "", nil); roundTrip.Code != http.StatusOK || roundTrip.Body.String() != "updated" {
		t.Fatalf("href round trip = %d %q", roundTrip.Code, roundTrip.Body.String())
	}
	if deleted := performDAVRequest(handler, http.MethodDelete, target, "", nil); deleted.Code != http.StatusNoContent {
		t.Fatalf("encoded DELETE status = %d", deleted.Code)
	}
	if victim := performDAVRequest(handler, http.MethodGet, "/victim.txt", "", nil); victim.Code != http.StatusOK || victim.Body.String() != "safe" {
		t.Fatalf("victim after encoded DELETE = %d %q", victim.Code, victim.Body.String())
	}
}

func TestHandlerPartialTreeMutationClearsOnlyUnmappedLocks(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodDelete, "MOVE"} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			locks, _ := newTestLockStore(t)
			objects := &failingDeleteObjects{memoryObjects: &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}}
			handler := Handler{
				Objects: objects, Locks: locks,
				Verify: func(context.Context, string, string) (Identity, error) {
					return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
				},
			}
			const first = "/source/a.txt"
			const failed = "/source/literal%252Fname.txt"
			for _, target := range []string{first, failed} {
				if created := performDAVRequest(handler, http.MethodPut, target, "data", nil); created.Code != http.StatusCreated {
					t.Fatalf("seed %s status = %d", target, created.Code)
				}
			}
			lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
			firstLock := performDAVRequest(handler, "LOCK", first, lockBody, map[string]string{"Depth": "0"})
			failedLock := performDAVRequest(handler, "LOCK", failed, lockBody, map[string]string{"Depth": "0"})
			firstToken := strings.Trim(firstLock.Header().Get("Lock-Token"), "<>")
			failedToken := strings.Trim(failedLock.Header().Get("Lock-Token"), "<>")
			if firstLock.Code != http.StatusOK || failedLock.Code != http.StatusOK || firstToken == "" || failedToken == "" {
				t.Fatalf("LOCK statuses = %d/%d", firstLock.Code, failedLock.Code)
			}
			objects.failSuffix = "source/literal%2Fname.txt"
			headers := map[string]string{
				"If": "<" + first + "> (<" + firstToken + ">) <" + failed + "> (<" + failedToken + ">)",
			}
			if method == "MOVE" {
				headers["Destination"] = "/target/"
			}
			response := performDAVRequest(handler, method, "/source/", "", headers)
			if response.Code != http.StatusMultiStatus {
				t.Fatalf("partial %s = %d body=%s", method, response.Code, response.Body.String())
			}
			var failures operationMultistatus
			if err := xml.Unmarshal(response.Body.Bytes(), &failures); err != nil || len(failures.Responses) == 0 || failures.Responses[0].Href != failed {
				t.Fatalf("partial %s failures = %#v, err=%v, body=%s", method, failures, err, response.Body.String())
			}
			if recreated := performDAVRequest(handler, http.MethodPut, first, "new", nil); recreated.Code != http.StatusCreated {
				t.Fatalf("PUT after successful source deletion = %d", recreated.Code)
			}
			if stillLocked := performDAVRequest(handler, http.MethodPut, failed, "blocked", nil); stillLocked.Code != http.StatusLocked {
				t.Fatalf("PUT on failed source member = %d", stillLocked.Code)
			}
		})
	}
}

func TestDeleteUnmappedLocksSkipsObjectLookupsWithoutRelevantLocks(t *testing.T) {
	t.Parallel()
	locks, _ := newTestLockStore(t)
	objects := &lookupCountingObjects{memoryObjects: &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}}
	handler := Handler{Objects: objects, Locks: locks, lockPrefix: "mount/"}

	if err := handler.deleteUnmappedLocks(context.Background(), []string{"tree/deleted.txt"}, nil); err != nil {
		t.Fatalf("deleteUnmappedLocks: %v", err)
	}
	if objects.statCalls != 0 || objects.listCalls != 0 {
		t.Fatalf("object lookups = stat:%d list:%d, want none", objects.statCalls, objects.listCalls)
	}
}

func TestHandlerRejectsFileCollectionCollisions(t *testing.T) {
	t.Parallel()
	objects := &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}
	handler := Handler{
		Objects: objects,
		Verify: func(context.Context, string, string) (Identity, error) {
			return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
		},
	}

	if created := performDAVRequest(handler, http.MethodPut, "/virtual/child.txt", "child", nil); created.Code != http.StatusCreated {
		t.Fatalf("seed virtual collection status = %d", created.Code)
	}
	if conflict := performDAVRequest(handler, http.MethodPut, "/virtual", "file", nil); conflict.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT on virtual collection status = %d", conflict.Code)
	}
	if _, exists := objects.metadata[r2.WebDAVMountPrefix("credential")+"virtual"]; exists {
		t.Fatal("PUT on virtual collection created an exact file")
	}

	if created := performDAVRequest(handler, http.MethodPut, "/file", "file", nil); created.Code != http.StatusCreated {
		t.Fatalf("seed file status = %d", created.Code)
	}
	if conflict := performDAVRequest(handler, "MKCOL", "/file", "", nil); conflict.Code != http.StatusMethodNotAllowed {
		t.Fatalf("MKCOL on file status = %d", conflict.Code)
	}
}

func TestHandlerRetainsVirtualParentCompatibility(t *testing.T) {
	t.Parallel()
	handler := conditionalTestHandler()

	file := performDAVRequest(handler, http.MethodPut, "/missing/parents/file.txt", "data", nil)
	if file.Code != http.StatusCreated {
		t.Fatalf("deep PUT status = %d, body = %s", file.Code, file.Body.String())
	}
	collection := performDAVRequest(handler, "MKCOL", "/other/missing/collection/", "", nil)
	if collection.Code != http.StatusCreated {
		t.Fatalf("deep MKCOL status = %d, body = %s", collection.Code, collection.Body.String())
	}
}

func TestHandlerLegacyFileCollectionCollisionDoesNotMutateEitherTree(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodDelete, "COPY", "MOVE"} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			objects := &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}
			handler := Handler{
				Objects: objects,
				Verify: func(context.Context, string, string) (Identity, error) {
					return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
				},
			}
			if created := performDAVRequest(handler, http.MethodPut, "/collision", "file", nil); created.Code != http.StatusCreated {
				t.Fatalf("seed file status = %d", created.Code)
			}
			mount := r2.WebDAVMountPrefix("credential")
			if _, err := objects.PutConditional(context.Background(), r2.PutRequest{
				Key: mount + "collision/child.txt", Body: strings.NewReader("child"), Size: 5,
			}); err != nil {
				t.Fatal(err)
			}

			headers := map[string]string{}
			if method != http.MethodDelete {
				headers["Destination"] = "/destination"
			}
			response := performDAVRequest(handler, method, "/collision", "", headers)
			if response.Code != http.StatusConflict {
				t.Fatalf("%s collision status = %d, body = %s", method, response.Code, response.Body.String())
			}
			if _, ok := objects.metadata[mount+"collision"]; !ok {
				t.Fatal("exact file was removed")
			}
			if _, ok := objects.metadata[mount+"collision/child.txt"]; !ok {
				t.Fatal("collection child was removed")
			}
			if _, ok := objects.metadata[mount+"destination"]; ok {
				t.Fatal("collision operation created a destination")
			}
		})
	}
}

func TestHandlerNestedDepthZeroLocksProtectTreeMembership(t *testing.T) {
	t.Parallel()
	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	newHandler := func(t *testing.T) Handler {
		t.Helper()
		locks, _ := newTestLockStore(t)
		handler := conditionalTestHandler()
		handler.Locks = locks
		return handler
	}
	lock := func(t *testing.T, handler Handler, target string) {
		t.Helper()
		response := performDAVRequest(handler, "LOCK", target, lockBody, map[string]string{"Depth": "0"})
		if response.Code != http.StatusOK {
			t.Fatalf("LOCK %s status = %d, body = %s", target, response.Code, response.Body.String())
		}
	}

	t.Run("delete ancestor", func(t *testing.T) {
		handler := newHandler(t)
		performDAVRequest(handler, http.MethodPut, "/tree/nested/file.txt", "data", nil)
		lock(t, handler, "/tree/nested/")
		response := performDAVRequest(handler, http.MethodDelete, "/tree/", "", nil)
		if response.Code != http.StatusLocked {
			t.Fatalf("DELETE ancestor status = %d, body = %s", response.Code, response.Body.String())
		}
		if child := performDAVRequest(handler, http.MethodGet, "/tree/nested/file.txt", "", nil); child.Code != http.StatusOK {
			t.Fatalf("locked child after DELETE status = %d", child.Code)
		}
	})

	t.Run("overwrite destination tree", func(t *testing.T) {
		handler := newHandler(t)
		performDAVRequest(handler, http.MethodPut, "/source/new.txt", "new", nil)
		performDAVRequest(handler, http.MethodPut, "/target/nested/stale.txt", "stale", nil)
		lock(t, handler, "/target/nested/")
		response := performDAVRequest(handler, "COPY", "/source/", "", map[string]string{"Destination": "/target/"})
		if response.Code != http.StatusLocked {
			t.Fatalf("COPY over locked subtree status = %d, body = %s", response.Code, response.Body.String())
		}
		if stale := performDAVRequest(handler, http.MethodGet, "/target/nested/stale.txt", "", nil); stale.Code != http.StatusOK {
			t.Fatalf("locked destination child status = %d", stale.Code)
		}
	})

	t.Run("move ancestor", func(t *testing.T) {
		handler := newHandler(t)
		performDAVRequest(handler, http.MethodPut, "/move/nested/file.txt", "data", nil)
		lock(t, handler, "/move/nested/")
		response := performDAVRequest(handler, "MOVE", "/move/", "", map[string]string{"Destination": "/moved/"})
		if response.Code != http.StatusLocked {
			t.Fatalf("MOVE ancestor status = %d, body = %s", response.Code, response.Body.String())
		}
		if child := performDAVRequest(handler, http.MethodGet, "/move/nested/file.txt", "", nil); child.Code != http.StatusOK {
			t.Fatalf("locked source child status = %d", child.Code)
		}
	})
}

func TestHandlerDepthZeroLocksCoverVirtualAncestorLifecycle(t *testing.T) {
	t.Parallel()
	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	t.Run("create virtual child collection", func(t *testing.T) {
		locks, _ := newTestLockStore(t)
		handler := conditionalTestHandler()
		handler.Locks = locks
		if created := performDAVRequest(handler, "MKCOL", "/docs/", "", nil); created.Code != http.StatusCreated {
			t.Fatalf("MKCOL docs status = %d", created.Code)
		}
		if locked := performDAVRequest(handler, "LOCK", "/docs/", lockBody, map[string]string{"Depth": "0"}); locked.Code != http.StatusOK {
			t.Fatalf("LOCK docs status = %d", locked.Code)
		}
		created := performDAVRequest(handler, http.MethodPut, "/docs/new/deep.txt", "data", nil)
		if created.Code != http.StatusLocked {
			t.Fatalf("deep PUT creating direct virtual member status = %d, body = %s", created.Code, created.Body.String())
		}
	})

	t.Run("delete virtual ancestor", func(t *testing.T) {
		locks, _ := newTestLockStore(t)
		handler := conditionalTestHandler()
		handler.Locks = locks
		if created := performDAVRequest(handler, http.MethodPut, "/tree/a/b/file.txt", "data", nil); created.Code != http.StatusCreated {
			t.Fatalf("seed deep file status = %d", created.Code)
		}
		if locked := performDAVRequest(handler, "LOCK", "/tree/a", lockBody, map[string]string{"Depth": "0"}); locked.Code != http.StatusOK {
			t.Fatalf("LOCK virtual ancestor status = %d", locked.Code)
		}
		deleted := performDAVRequest(handler, http.MethodDelete, "/tree/", "", nil)
		if deleted.Code != http.StatusLocked {
			t.Fatalf("DELETE virtual ancestor tree status = %d, body = %s", deleted.Code, deleted.Body.String())
		}
	})
}

func TestHandlerTreeMutationRejectsPhantomChild(t *testing.T) {
	t.Parallel()
	base := &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}
	mount := r2.WebDAVMountPrefix("credential")
	if _, err := base.PutConditional(context.Background(), r2.PutRequest{
		Key: mount + "tree/old.txt", Body: strings.NewReader("old"), Size: 3,
	}); err != nil {
		t.Fatal(err)
	}
	objects := &injectingListObjects{memoryObjects: base, injectOnCall: 3, injectKey: mount + "tree/new.txt"}
	handler := Handler{
		Objects: objects,
		Verify: func(context.Context, string, string) (Identity, error) {
			return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
		},
	}
	response := performDAVRequest(handler, http.MethodDelete, "/tree/", "", nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("DELETE after phantom child status = %d, body = %s", response.Code, response.Body.String())
	}
	for _, target := range []string{"/tree/old.txt", "/tree/new.txt"} {
		if get := performDAVRequest(handler, http.MethodGet, target, "", nil); get.Code != http.StatusOK {
			t.Fatalf("%s after rejected DELETE status = %d", target, get.Code)
		}
	}
}

func TestHandlerVirtualCollectionMoveDeleteFailureReturnsMultistatus(t *testing.T) {
	t.Parallel()
	objects := &failingDeleteObjects{memoryObjects: &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)}}
	handler := Handler{
		Objects: objects,
		Verify: func(context.Context, string, string) (Identity, error) {
			return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
		},
	}
	if created := performDAVRequest(handler, http.MethodPut, "/source/child.txt", "child", nil); created.Code != http.StatusCreated {
		t.Fatalf("seed child status = %d", created.Code)
	}
	objects.failSuffix = "source/child.txt"

	moved := performDAVRequest(handler, "MOVE", "/source/", "", map[string]string{"Destination": "/target/"})
	if moved.Code != http.StatusMultiStatus || !strings.Contains(moved.Body.String(), "/source/child.txt") {
		t.Fatalf("partial MOVE = %d body = %s", moved.Code, moved.Body.String())
	}
	if source := performDAVRequest(handler, http.MethodGet, "/source/child.txt", "", nil); source.Code != http.StatusOK {
		t.Fatalf("source after failed delete status = %d", source.Code)
	}
	if target := performDAVRequest(handler, http.MethodGet, "/target/child.txt", "", nil); target.Code != http.StatusOK || target.Body.String() != "child" {
		t.Fatalf("copied target = %d %q", target.Code, target.Body.String())
	}
}

func TestHandlerDeleteAndMoveRemoveStaleLocks(t *testing.T) {
	t.Parallel()
	locks, _ := newTestLockStore(t)
	handler := conditionalTestHandler()
	handler.Locks = locks
	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`

	performDAVRequest(handler, http.MethodPut, "/delete.txt", "delete", nil)
	deleteLock := performDAVRequest(handler, "LOCK", "/delete.txt", lockBody, map[string]string{"Depth": "0"})
	deleteToken := strings.Trim(deleteLock.Header().Get("Lock-Token"), "<>")
	deleted := performDAVRequest(handler, http.MethodDelete, "/delete.txt", "", map[string]string{"If": "(<" + deleteToken + ">)"})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("conditional DELETE status = %d", deleted.Code)
	}
	if recreated := performDAVRequest(handler, http.MethodPut, "/delete.txt", "new", nil); recreated.Code != http.StatusCreated {
		t.Fatalf("PUT after DELETE status = %d", recreated.Code)
	}

	performDAVRequest(handler, http.MethodPut, "/move.txt", "move", nil)
	moveLock := performDAVRequest(handler, "LOCK", "/move.txt", lockBody, map[string]string{"Depth": "0"})
	moveToken := strings.Trim(moveLock.Header().Get("Lock-Token"), "<>")
	moved := performDAVRequest(handler, "MOVE", "/move.txt", "", map[string]string{
		"Destination": "/moved.txt", "If": "(<" + moveToken + ">)",
	})
	if moved.Code != http.StatusCreated {
		t.Fatalf("conditional MOVE status = %d, body = %s", moved.Code, moved.Body.String())
	}
	if recreated := performDAVRequest(handler, http.MethodPut, "/move.txt", "new", nil); recreated.Code != http.StatusCreated {
		t.Fatalf("PUT after MOVE status = %d", recreated.Code)
	}
}

func conditionalTestHandler() Handler {
	return Handler{
		Objects: &memoryObjects{values: make(map[string][]byte), metadata: make(map[string]r2.Object)},
		Verify: func(context.Context, string, string) (Identity, error) {
			return Identity{ID: "credential", Scopes: []string{"r2:*"}}, nil
		},
	}
}

func performDAVRequest(handler Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.SetBasicAuth("dav", "secret")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type failingGetObjects struct {
	*memoryObjects
	failSuffix string
}

func (s *failingGetObjects) Get(ctx context.Context, key string, options r2.GetOptions) (r2.GetResult, error) {
	if s.failSuffix != "" && strings.HasSuffix(key, s.failSuffix) {
		return r2.GetResult{}, r2.ErrConditionalRequestConflict
	}
	return s.memoryObjects.Get(ctx, key, options)
}

type recordingGetObjects struct {
	*memoryObjects
	lastOptions          r2.GetOptions
	physicalLastModified time.Time
}

type sameETagReplacingObjects struct {
	*memoryObjects
	modified  time.Time
	statCalls int
	getCalls  int
}

func (s *sameETagReplacingObjects) Stat(_ context.Context, key string) (r2.Object, error) {
	s.statCalls++
	objectID := "first-object"
	modified := s.modified
	if s.statCalls > 1 {
		objectID = "second-object"
		modified = modified.Add(time.Hour)
	}
	return r2.Object{
		Key: key, ObjectID: objectID, Size: 6, ETag: "shared", LastModified: modified,
	}, nil
}

func (s *sameETagReplacingObjects) Get(_ context.Context, _ string, options r2.GetOptions) (r2.GetResult, error) {
	s.getCalls++
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

type firstStatOverrideObjects struct {
	*memoryObjects
	firstExists bool
	statCalls   int
	putCalls    int
}

type injectingListObjects struct {
	*memoryObjects
	listCalls    int
	injectOnCall int
	injectKey    string
}

func (s *injectingListObjects) List(ctx context.Context, options r2.ListOptions) (r2.ObjectList, error) {
	s.listCalls++
	if s.listCalls == s.injectOnCall {
		_, _ = s.memoryObjects.PutConditional(ctx, r2.PutRequest{
			Key: s.injectKey, Body: strings.NewReader("new"), Size: 3,
		})
	}
	return s.memoryObjects.List(ctx, options)
}

func (s *firstStatOverrideObjects) Stat(ctx context.Context, key string) (r2.Object, error) {
	s.statCalls++
	if s.statCalls == 1 {
		if s.firstExists {
			return r2.Object{Key: key, ETag: "stale", LastModified: time.Now()}, nil
		}
		return r2.Object{}, r2.ErrObjectNotFound
	}
	return s.memoryObjects.Stat(ctx, key)
}

func (s *firstStatOverrideObjects) PutConditional(ctx context.Context, request r2.PutRequest) (r2.PutResult, error) {
	s.putCalls++
	return s.memoryObjects.PutConditional(ctx, request)
}

type failingDeleteObjects struct {
	*memoryObjects
	failSuffix string
}

type lookupCountingObjects struct {
	*memoryObjects
	statCalls int
	listCalls int
}

func (s *lookupCountingObjects) Stat(ctx context.Context, key string) (r2.Object, error) {
	s.statCalls++
	return s.memoryObjects.Stat(ctx, key)
}

func (s *lookupCountingObjects) List(ctx context.Context, options r2.ListOptions) (r2.ObjectList, error) {
	s.listCalls++
	return s.memoryObjects.List(ctx, options)
}

func (s *failingDeleteObjects) DeleteConditional(ctx context.Context, key string, conditions r2.MutationConditions) error {
	if s.failSuffix != "" && strings.HasSuffix(key, s.failSuffix) {
		return r2.ErrConditionalRequestConflict
	}
	return s.memoryObjects.DeleteConditional(ctx, key, conditions)
}

func (s *recordingGetObjects) Get(ctx context.Context, key string, options r2.GetOptions) (r2.GetResult, error) {
	s.lastOptions = options
	result, err := s.memoryObjects.Get(ctx, key, options)
	if err == nil {
		result.LastModified = s.physicalLastModified
	}
	return result, err
}
