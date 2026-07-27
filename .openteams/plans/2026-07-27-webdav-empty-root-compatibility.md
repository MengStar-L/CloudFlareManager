# WebDAV Empty Root Compatibility Implementation Plan

**Goal:** Let RaiDrive validate and mount an empty WebDAV namespace by exposing a temporary `/.empty/` collection without persisting an R2 object.

**Architecture:** Keep the compatibility behavior inside `Handler.properties`, where WebDAV resources are already synthesized. Root Depth 1 listings add the virtual child only after a successful empty object-index listing; direct discovery of `/.empty/` confirms the whole namespace is empty before returning it as a collection. All storage and non-PROPFIND behavior remains unchanged.

**Tech Stack:** Go 1.26, `net/http`, `encoding/xml`, `net/http/httptest`, existing in-memory WebDAV object service.

---

### Task 1: Capture empty-root behavior in tests

**Files:**
- Modify: `internal/protocol/webdav/handler_test.go:16`
- Test: `internal/protocol/webdav/handler_test.go`

- **Step 1: Add a test helper that performs authenticated PROPFIND requests**

Add a local helper so each assertion exercises the real `ServeHTTP` authentication, path parsing, and dispatch flow:

```go
func performPropfind(handler Handler, target, depth string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("PROPFIND", target, nil)
	request.SetBasicAuth("dav", "secret")
	request.Header.Set("Depth", depth)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
```

- **Step 2: Write the empty-namespace regression test**

Create `TestHandlerPropfindEmptyRootCompatibility`. Use an empty `memoryObjects`, the same verifier contract as the existing test, and XML decoding into `multistatus`. Assert these exact cases:

```go
rootDepthOne := performPropfind(handler, "/", "1")
// Status is 207. The decoded response contains "/" and "/.empty/",
// and both have a non-nil ResourceType.Collection.

rootDepthZero := performPropfind(handler, "/", "0")
// Status is 207. The decoded response contains only "/".

placeholder := performPropfind(handler, "/.empty/", "0")
// Status is 207. The decoded response contains only "/.empty/" as a collection.
```

Then insert one committed object into `memoryObjects` and assert:

```go
withRealObject := performPropfind(handler, "/", "1")
// The response contains "/readme.txt" and does not contain "/.empty/".

missingPlaceholder := performPropfind(handler, "/.empty/", "0")
// Status is 404 because the synthetic collection no longer exists.
```

- **Step 3: Run the new test and verify that it fails for the intended reason**

Run:

```powershell
go test ./internal/protocol/webdav -run TestHandlerPropfindEmptyRootCompatibility -count=1 -v
```

Expected: FAIL because an empty root currently has no `/.empty/` child and direct discovery returns 404.

### Task 2: Add the virtual collection to WebDAV discovery

**Files:**
- Modify: `internal/protocol/webdav/handler.go:208`
- Test: `internal/protocol/webdav/handler_test.go`

- **Step 1: Define the compatibility path next to the WebDAV handler types**

Add one package-local constant:

```go
const virtualEmptyCollection = ".empty/"
```

- **Step 2: Recognize direct discovery while the whole namespace is empty**

At the start of `Handler.properties`, before ordinary object lookup, handle the virtual key with a bounded root listing:

```go
if key == virtualEmptyCollection {
	objects, err := h.Objects.List(ctx, r2.ListOptions{Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(objects.Objects) == 0 {
		return []propertyResponse{makeProperty(key, r2.Object{}, true)}, nil
	}
}
```

If real content exists, continue through existing lookup logic. This preserves a real `.empty/` implicit directory while returning 404 for a purely synthetic placeholder that should have disappeared.

- **Step 3: Add the virtual child to an empty root Depth 1 listing**

After the existing root list succeeds and before iterating over children, append the synthetic collection only for an empty root result:

```go
if key == "" && len(list.Objects) == 0 {
	responses = append(responses, makeProperty(virtualEmptyCollection, r2.Object{}, true))
}
```

Do not add it for Depth 0 because `properties` already returns before listing children. Do not convert listing errors into empty results.

- **Step 4: Format and rerun the focused test**

Run:

```powershell
gofmt -w internal/protocol/webdav/handler.go internal/protocol/webdav/handler_test.go
go test ./internal/protocol/webdav -run TestHandlerPropfindEmptyRootCompatibility -count=1 -v
```

Expected: PASS.

### Task 3: Verify existing behavior and repository health

**Files:**
- Verify: `internal/protocol/webdav/handler.go`
- Verify: `internal/protocol/webdav/handler_test.go`

- **Step 1: Run all WebDAV protocol tests**

Run:

```powershell
go test ./internal/protocol/webdav -count=1
```

Expected: PASS, including the existing PUT, GET, and non-empty PROPFIND workflow.

- **Step 2: Run the complete Go test suite**

Run:

```powershell
go test ./... -count=1
```

Expected: PASS for every Go package.

- **Step 3: Inspect the final patch**

Run:

```powershell
git diff --check
git status --short
git diff -- internal/protocol/webdav/handler.go internal/protocol/webdav/handler_test.go
```

Expected: no whitespace errors; only the approved design document, this plan, and the two WebDAV source files are changed. Do not create a commit unless the user requests one.
