# WebDAV Empty Root Compatibility Implementation Plan

**Goal:** Let RaiDrive validate and mount an empty WebDAV namespace by returning the existing root collection with a valid `/` display name and no synthetic child.

**Architecture:** Keep the compatibility correction inside `makeProperty`, where WebDAV display names are already generated. Special-case only the root href, remove the v0.1.5 `/.empty/` discovery behavior, and preserve all storage, authentication, and non-root property behavior.

**Tech Stack:** Go 1.26, `net/http`, `encoding/xml`, `net/http/httptest`, existing in-memory WebDAV object service, GitHub Actions release workflow, RaiDrive 2025.12.30.

---

### Task 1: Replace the incorrect placeholder test with a root-property regression

**Files:**
- Modify: `internal/protocol/webdav/handler_test.go:57`
- Test: `internal/protocol/webdav/handler_test.go`

- **Step 1: Make the PROPFIND assertion return decoded responses**

Replace the existing collection-only helper inside `TestHandlerPropfindEmptyRootCompatibility` with a path assertion that returns the decoded response slice:

```go
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
```

- **Step 2: Assert a valid empty root without synthetic children**

Replace the placeholder expectations with exact root behavior:

```go
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
```

After seeding `readme.txt`, assert that the root and real child are returned, the root display name remains `/`, and the response does not contain `/.empty/`:

```go
withRealObject := performPropfind(handler, "/", "1")
entries := assertResponses(withRealObject, "/", "/readme.txt")
if entries[0].PropStat.Properties.DisplayName != "/" ||
	entries[1].PropStat.Properties.DisplayName != "readme.txt" ||
	entries[1].PropStat.Properties.ResourceType.Collection != nil ||
	strings.Contains(withRealObject.Body.String(), "/.empty/") {
	t.Fatalf("PROPFIND response with real object = %s", withRealObject.Body.String())
}
```

- **Step 3: Run the focused regression and verify the old implementation fails**

Run:

```powershell
go test ./internal/protocol/webdav -run TestHandlerPropfindEmptyRootCompatibility -count=1 -v
```

Expected: FAIL because v0.1.5 returns `displayname=.` for `/`, adds `/.empty/` to an empty Depth 1 listing, and resolves the synthetic path.

### Task 2: Correct root properties and remove the virtual collection

**Files:**
- Modify: `internal/protocol/webdav/handler.go:49`
- Modify: `internal/protocol/webdav/handler.go:210`
- Modify: `internal/protocol/webdav/handler.go:480`
- Test: `internal/protocol/webdav/handler_test.go`

- **Step 1: Remove the virtual collection identifier**

Delete:

```go
const virtualEmptyCollection = ".empty/"
```

- **Step 2: Remove direct synthetic collection discovery**

Delete the opening block in `Handler.properties` that lists the root and returns `makeProperty(virtualEmptyCollection, r2.Object{}, true)`.

- **Step 3: Stop adding a child to empty root listings**

Delete:

```go
if key == "" && len(list.Objects) == 0 {
	responses = append(responses, makeProperty(virtualEmptyCollection, r2.Object{}, true))
}
```

- **Step 4: Give the root resource a valid display name**

In `makeProperty`, calculate the display name once and special-case only the normalized root href:

```go
displayName := path.Base(strings.TrimSuffix(href, "/"))
if href == "/" {
	displayName = "/"
}
```

Use `displayName` in the returned properties:

```go
DisplayName: displayName, ResourceType: resourceType,
```

- **Step 5: Format and rerun the focused test**

Run:

```powershell
gofmt -w internal/protocol/webdav/handler.go internal/protocol/webdav/handler_test.go
go test ./internal/protocol/webdav -run TestHandlerPropfindEmptyRootCompatibility -count=1 -v
```

Expected: PASS.

### Task 3: Verify protocol and repository health

**Files:**
- Verify: `internal/protocol/webdav/handler.go`
- Verify: `internal/protocol/webdav/handler_test.go`

- **Step 1: Run all WebDAV protocol tests**

Run:

```powershell
go test ./internal/protocol/webdav -count=1 -v
```

Expected: PASS, including PUT, GET, non-empty PROPFIND, and empty-root compatibility.

- **Step 2: Run the complete Go test suite**

Run:

```powershell
go test ./... -count=1
```

Expected: PASS for every Go package.

- **Step 3: Build all Go packages**

Run:

```powershell
go build ./...
```

Expected: exit code 0.

- **Step 4: Inspect the final patch**

Run:

```powershell
git diff --check
git status --short
git diff -- internal/protocol/webdav/handler.go internal/protocol/webdav/handler_test.go .openteams/plans/2026-07-27-webdav-empty-root-compatibility.md
```

Expected: no whitespace errors; changes are limited to the corrected plan, WebDAV handler, and WebDAV tests.

### Task 4: Commit and trigger the patch release

**Files:**
- Commit: `.openteams/plans/2026-07-27-webdav-empty-root-compatibility.md`
- Commit: `internal/protocol/webdav/handler.go`
- Commit: `internal/protocol/webdav/handler_test.go`
- Verify: `.github/workflows/release.yml`

- **Step 1: Commit the implementation**

Run:

```powershell
git add -- .openteams/plans/2026-07-27-webdav-empty-root-compatibility.md internal/protocol/webdav/handler.go internal/protocol/webdav/handler_test.go
git commit -m "Fix WebDAV root discovery for RaiDrive"
```

Expected: one commit containing only the implementation plan, handler, and regression test.

- **Step 2: Create the next patch tag**

Run:

```powershell
git tag -a v0.1.6 -m "v0.1.6"
```

Expected: annotated tag `v0.1.6` points to the implementation commit.

- **Step 3: Push main and the release tag**

Run:

```powershell
git push origin main
git push origin v0.1.6
```

Expected: both pushes succeed and the tag triggers `.github/workflows/release.yml`.

- **Step 4: Wait for the release workflow**

Run:

```powershell
$webdavReleaseRunId = gh run list --workflow release.yml --limit 1 --json databaseId,headBranch --jq '.[] | select(.headBranch == "v0.1.6") | .databaseId'
gh run watch $webdavReleaseRunId --exit-status
```

Expected: the `v0.1.6` run completes with conclusion `success`.

### Task 5: Verify deployment and dogfood the real client

**Files:**
- Verify: production service at `https://cfmanager.704255803.xyz/healthz`
- Verify: RaiDrive 2025.12.30 `test` connection

- **Step 1: Verify the production version**

Run:

```powershell
curl.exe -fsS https://cfmanager.704255803.xyz/healthz
```

Expected after the updater completes:

```json
{"status":"ok","version":"v0.1.6"}
```

- **Step 2: Connect the saved production RaiDrive entry**

Open the existing `test` WebDAV entry at `https://dav.704255803.xyz/`, click Connect, and wait for the connection result. Do not reveal, replace, or log the saved password.

Expected: no `ValidateUrl : Not found file` dialog; the `test` entry reports connected and the drive is mounted.

- **Step 3: Confirm the production handshake in RaiDrive logs**

Read the latest entries from:

```text
C:\ProgramData\OpenBoxLab\RaiDrive Mount\log\service.log
```

Expected: the latest `WebDAV(test)` authentication and connection sequence succeeds without `ValidateUrl`, `Auth Failed`, or `Connect Finally Failed`.

- **Step 4: Verify repository and release state**

Run:

```powershell
git status --short --branch
git log -3 --oneline --decorate
gh release view v0.1.6 --json url,tagName,isDraft,isPrerelease
```

Expected: clean `main` synchronized with `origin/main`, `v0.1.6` on the implementation commit, and a published non-draft release.
