# WebDAV Mount Isolation Implementation Plan

**Goal:** Make every WebDAV credential an isolated virtual folder in the file manager, migrate all legacy global files into the existing `gamesync` mount, and remove the root summary band.

**Architecture:** Store WebDAV files under a reserved logical-key prefix derived from the credential ID while keeping physical R2 object mappings unchanged. A scoped WebDAV object adapter and scoped admin file APIs translate visible paths to internal paths; the file-manager root is built from WebDAV credentials rather than object prefixes. A one-time startup transaction rewrites legacy logical keys into the oldest WebDAV credential, which is `gamesync` in the current installation.

**Tech Stack:** Go 1.24, SQLite, `net/http`, React 19, TypeScript, Vite, Vitest-free type/build verification, browser integration testing.

---

## File Map

- Create `internal/modules/r2/webdav_mounts.go`: reserved-prefix helpers, scoped directory listing, and one-time logical-key migration.
- Create `internal/modules/r2/webdav_mounts_test.go`: namespace validation, migration, rollback, and idempotency tests.
- Create `internal/protocol/webdav/scoped_objects.go`: per-credential `ObjectService` adapter that prefixes requests and strips returned keys.
- Modify `internal/protocol/webdav/handler.go`: install the scoped adapter after authentication and scope WebDAV locks.
- Modify `internal/protocol/webdav/handler_test.go`: verify two credentials cannot see each other's files and generated hrefs remain relative.
- Modify `internal/protocol/s3/handler.go`: reject and filter the reserved WebDAV namespace.
- Modify `internal/protocol/s3/multipart.go`: hide reserved multipart uploads.
- Modify `internal/protocol/s3/handler_test.go` and `internal/protocol/s3/multipart_test.go`: verify reserved keys are inaccessible.
- Modify `internal/platform/httpapi/files.go`: virtual mount root and mount-scoped file operations.
- Modify `internal/platform/httpapi/files_test.go`: root mount listing, mount isolation, empty mount, and cross-mount rejection tests.
- Modify `internal/platform/httpapi/api.go`: protect deletion of non-empty WebDAV credential records and finish deferred migration after first credential creation.
- Modify `internal/platform/httpapi/api_test.go`: credential lifecycle protection tests.
- Modify `internal/app/server.go`: run the one-time migration before jobs and listeners and pass stable credential IDs to WebDAV.
- Modify `web/src/types.ts`: mount entry and mount metadata response types.
- Modify `web/src/App.tsx`: hash route state for `mount` plus relative `path`.
- Modify `web/src/pages/FilesPage.tsx`: render virtual mounts at root and scope every action to a mount.
- Modify `web/src/components/FileManagerDialogs.tsx`: include `mount_id` in preview, download, and move-directory requests.
- Modify `web/src/styles/pages.css` and `web/src/styles/responsive.css`: remove summary-band layout and style disabled mount rows without overlap.

### Task 1: Define WebDAV namespace primitives

**Files:**
- Create: `internal/modules/r2/webdav_mounts.go`
- Create: `internal/modules/r2/webdav_mounts_test.go`

- **Step 1: Write failing namespace tests**

Add table tests for stable prefix generation, visible-path validation, and prefix stripping:

```go
func TestWebDAVMountPath(t *testing.T) {
	prefix := WebDAVMountPrefix("credential-id")
	if prefix != ".cf-r2-manager/webdav/credential-id/" {
		t.Fatalf("prefix = %q", prefix)
	}
	key, err := WebDAVMountKey("credential-id", "saves/slot.dat")
	if err != nil || key != prefix+"saves/slot.dat" {
		t.Fatalf("key = %q, err = %v", key, err)
	}
	visible, ok := WebDAVVisibleKey("credential-id", key)
	if !ok || visible != "saves/slot.dat" {
		t.Fatalf("visible = %q, ok = %v", visible, ok)
	}
}

func TestWebDAVMountKeyRejectsTraversal(t *testing.T) {
	for _, value := range []string{"../secret", "/absolute", `bad\\key`, "a//b"} {
		if _, err := WebDAVMountKey("credential-id", value); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("value %q: err = %v", value, err)
		}
	}
}
```

- **Step 2: Run the focused tests and verify failure**

Run: `go test ./internal/modules/r2 -run 'TestWebDAVMount' -count=1`

Expected: FAIL because `WebDAVMountPrefix`, `WebDAVMountKey`, and `WebDAVVisibleKey` do not exist.

- **Step 3: Implement the minimal namespace API**

Define:

```go
const WebDAVNamespaceRoot = ".cf-r2-manager/webdav/"

func WebDAVMountPrefix(credentialID string) string
func IsWebDAVInternalKey(key string) bool
func WebDAVMountKey(credentialID, visible string) (string, error)
func WebDAVVisibleKey(credentialID, internal string) (string, bool)
```

Validate the visible path with the existing logical-path rules before adding the internal prefix. Permit an empty visible path only as the virtual mount root. Reject empty credential IDs and IDs containing `/`, `\\`, control characters, `.` or `..` segments. Keep the external 1024-byte limit on the visible key rather than the prefixed key.

- **Step 4: Run the focused tests and verify success**

Run: `go test ./internal/modules/r2 -run 'TestWebDAVMount' -count=1`

Expected: PASS.

- **Step 5: Commit the namespace primitives**

```bash
git add internal/modules/r2/webdav_mounts.go internal/modules/r2/webdav_mounts_test.go
git commit -m "feat: define isolated WebDAV namespaces"
```

### Task 2: Migrate legacy logical keys into gamesync

**Files:**
- Modify: `internal/modules/r2/webdav_mounts.go`
- Modify: `internal/modules/r2/webdav_mounts_test.go`
- Modify: `internal/app/server.go`

- **Step 1: Write failing migration tests**

Use the existing SQLite test store helper to insert:

```text
GameSync/save.dat
claude.png
```

Also insert a `webdav_locks.object_key`, an `r2_multipart_uploads.object_key`, and a pending `r2.files.move` payload. Call:

```go
result, err := store.EnsureWebDAVNamespaces(ctx, []string{"gamesync-id", "test-id"})
```

Assert that:

- both objects become `.cf-r2-manager/webdav/gamesync-id/<old-key>`;
- the lock, multipart record, and file-job source/destination receive the same prefix;
- `result.TargetCredentialID == "gamesync-id"` and `result.MigratedObjects == 2`;
- the second call reports `AlreadyComplete` and changes nothing;
- a pre-existing key below `.cf-r2-manager/webdav/` without a completion marker returns `ErrWebDAVNamespaceConflict` and rolls back every table change;
- when legacy objects exist and the credential slice is empty, the result is deferred and no marker is written.

- **Step 2: Run migration tests and verify failure**

Run: `go test ./internal/modules/r2 -run 'TestEnsureWebDAVNamespaces' -count=1`

Expected: FAIL because the migration method and result type do not exist.

- **Step 3: Implement one transaction and one marker**

Add:

```go
const webDAVNamespaceMigrationSetting = "r2.webdav_namespace.v1"

type WebDAVNamespaceMigration struct {
	TargetCredentialID string
	MigratedObjects    int64
	AlreadyComplete    bool
	Deferred           bool
}

func (s *Store) EnsureWebDAVNamespaces(ctx context.Context, credentialIDsOldestFirst []string) (WebDAVNamespaceMigration, error)
```

Within one `sql.Tx`:

1. Return `AlreadyComplete` when the setting exists.
2. Count non-reserved rows in `r2_objects`; if zero, write the marker and commit.
3. Return `Deferred` without a marker when no WebDAV credential exists.
4. Fail before updates when any reserved-prefix row exists without the marker.
5. Prefix `r2_objects.object_key`, `r2_multipart_uploads.object_key`, and `webdav_locks.object_key`.
6. Decode pending/running `r2.files.move` and `r2.files.delete` job payloads, prefix non-empty source/destination values, and update `payload_json`.
7. Persist JSON containing the target credential ID and completion time in `system_settings`.

Do not copy physical R2 objects or alter `physical_key`.

- **Step 4: Wire migration before the runner starts**

In `internal/app/server.go`, list WebDAV credentials, reverse the current newest-first result into oldest-first IDs, and call `EnsureWebDAVNamespaces` after constructing `r2Store` but before enqueueing recovery or starting listeners. Log the target and migrated count. Return the error to stop startup on collision or partial failure.

- **Step 5: Run migration and app tests**

Run: `go test ./internal/modules/r2 ./internal/app -count=1`

Expected: PASS.

- **Step 6: Commit the migration**

```bash
git add internal/modules/r2/webdav_mounts.go internal/modules/r2/webdav_mounts_test.go internal/app/server.go
git commit -m "feat: migrate legacy files into the oldest WebDAV mount"
```

### Task 3: Scope every WebDAV request to its credential

**Files:**
- Create: `internal/protocol/webdav/scoped_objects.go`
- Modify: `internal/protocol/webdav/handler.go`
- Modify: `internal/protocol/webdav/handler_test.go`
- Modify: `internal/app/server.go`

- **Step 1: Write failing isolation tests**

Construct one handler whose verifier returns `gamesync-id` for one username and `test-id` for the other. Exercise PUT and PROPFIND with Basic Auth:

```go
putWebDAV(t, handler, "gamesync", "/same.txt", "game")
putWebDAV(t, handler, "test", "/same.txt", "test")
assertWebDAVBody(t, handler, "gamesync", "/same.txt", "game")
assertWebDAVBody(t, handler, "test", "/same.txt", "test")
```

Assert that an empty `test` root PROPFIND returns 207, that listings never contain `.cf-r2-manager`, and that a lock created by one credential does not block the same visible path in the other credential.

- **Step 2: Run the WebDAV tests and verify failure**

Run: `go test ./internal/protocol/webdav -run 'Test.*CredentialNamespace' -count=1`

Expected: FAIL because both identities still share raw object keys and lock keys.

- **Step 3: Implement the scoped adapter**

Create a private adapter:

```go
type scopedObjects struct {
	base   ObjectService
	prefix string
}
```

Implement all six `ObjectService` methods. Prefix every input key, `ListOptions.Prefix`, and non-empty `ListOptions.After`; strip the prefix from returned `Object.Key` values and `NextMarker`. Reject a returned key outside the prefix.

In `ServeHTTP`, after authentication:

```go
prefix := r2.WebDAVMountPrefix(identity.ID)
h.Objects = scopedObjects{base: h.Objects, prefix: prefix}
h.lockPrefix = prefix
```

Keep request parsing, href generation, and directory aggregation on visible paths. Prefix only calls to `LockStore.Check` and `LockStore.Create`; token refresh and deletion remain token-based.

- **Step 4: Run all WebDAV tests**

Run: `go test ./internal/protocol/webdav -count=1`

Expected: PASS, including existing empty-root and RaiDrive discovery tests.

- **Step 5: Commit WebDAV isolation**

```bash
git add internal/protocol/webdav/scoped_objects.go internal/protocol/webdav/handler.go internal/protocol/webdav/handler_test.go internal/app/server.go
git commit -m "feat: isolate WebDAV credentials by namespace"
```

### Task 4: Hide WebDAV namespaces from S3

**Files:**
- Modify: `internal/protocol/s3/handler.go`
- Modify: `internal/protocol/s3/multipart.go`
- Modify: `internal/protocol/s3/handler_test.go`
- Modify: `internal/protocol/s3/multipart_test.go`

- **Step 1: Write failing reserved-key tests**

Add tests that seed one ordinary key and one `.cf-r2-manager/webdav/credential/file.txt` key. Verify:

- root and delimited S3 listings contain only the ordinary key;
- GET, HEAD, PUT, DELETE, COPY source, and COPY destination for a reserved key return `403 AccessDenied`;
- multipart creation under the reserved prefix returns 403;
- existing reserved multipart uploads are omitted from the global multipart listing.

- **Step 2: Run S3 tests and verify failure**

Run: `go test ./internal/protocol/s3 -run 'Test.*WebDAVNamespace' -count=1`

Expected: FAIL because reserved keys are currently treated as ordinary S3 keys.

- **Step 3: Add request and listing guards**

After `splitPath`, reject non-empty keys for which `r2.IsWebDAVInternalKey(key)` is true. Apply the same check to copy sources. In `listObjectEntries`, skip reserved objects while continuing through source pages until the requested visible limit is filled. If the requested prefix itself is reserved, return an empty list rather than revealing existence.

In multipart listing, page through the service and omit uploads whose keys are reserved; preserve the visible `max-uploads` limit and continuation marker semantics.

- **Step 4: Run all S3 tests**

Run: `go test ./internal/protocol/s3 -count=1`

Expected: PASS.

- **Step 5: Commit S3 protection**

```bash
git add internal/protocol/s3/handler.go internal/protocol/s3/multipart.go internal/protocol/s3/handler_test.go internal/protocol/s3/multipart_test.go
git commit -m "fix: hide WebDAV mounts from S3"
```

### Task 5: Expose a virtual mount root in the admin API

**Files:**
- Modify: `internal/modules/r2/file_manager.go`
- Modify: `internal/modules/r2/file_manager_test.go`
- Modify: `internal/platform/httpapi/files.go`
- Modify: `internal/platform/httpapi/files_test.go`
- Modify: `internal/platform/httpapi/api.go`
- Modify: `internal/platform/httpapi/api_test.go`

- **Step 1: Write failing scoped-directory tests**

Add `Service.ListWebDAVDirectory(ctx, credentialID, options)` tests where `gamesync-id` has `GameSync/save.dat` and `claude.png`, while `test-id` has `other.txt`. Assert each result contains only its own entries, keys are visible relative keys, and an empty mount root returns 200-equivalent data rather than `ErrObjectNotFound`.

- **Step 2: Implement scoped file-manager helpers**

Add methods that validate the visible path, add the mount prefix, delegate to existing file operations, and strip the prefix from result entries:

```go
func (s Service) ListWebDAVDirectory(ctx context.Context, credentialID string, options DirectoryListOptions) (DirectoryList, error)
func (s Service) ResolveWebDAVEntry(ctx context.Context, credentialID, key string) (FileEntry, error)
func (s Service) CreateWebDAVDirectory(ctx context.Context, credentialID, key string) (FileEntry, error)
```

The mount root bypasses `ResolveEntry` because it is virtual and may contain zero objects. File upload, move, delete, and job enqueue paths use `WebDAVMountKey` directly after validating visible paths.

- **Step 3: Write failing HTTP root and isolation tests**

Seed WebDAV credentials named `gamesync` and `test`. Verify:

```text
GET /api/v1/files
  -> entries: [{kind:"mount", name:"gamesync"}, {kind:"mount", name:"test"}]
  -> no GameSync object folder and no claude.png at this level

GET /api/v1/files?mount_id=<gamesync-id>&path=
  -> GameSync directory and claude.png

GET /api/v1/files?mount_id=<test-id>&path=
  -> empty list
```

Also verify missing/AI/S3 mount IDs return 404, path without a mount ID returns 400, and upload/move/delete/content requests cannot address another mount's internal key.

- **Step 4: Implement virtual root responses**

Extend `r2.EntryKind` with `EntryMount = "mount"` and `FileEntry` with optional fields:

```go
MountID  string `json:"mount_id,omitempty"`
Disabled bool   `json:"disabled,omitempty"`
```

Extend `DirectoryList` with `MountID` and `MountName`. In `listFiles`, when `mount_id` is absent and `path` is empty, list only WebDAV credentials, sort by case-folded name plus ID, and paginate with an opaque cursor. When `mount_id` is present, load that credential, require kind `webdav`, and call the scoped directory method.

Populate `mount_id` on every file and directory entry returned from a scoped listing or resolve call. Root mount entries use an empty visible `key` and their own credential ID in `mount_id`, so the frontend never derives identity from a display name.

Require `mount_id` on content, upload, directory, and operation endpoints. Keep all client-facing keys and audit resources relative; map to internal keys immediately before calling R2 services or enqueueing jobs.

- **Step 5: Protect credential deletion**

Before deleting a revoked WebDAV credential record, call `CountObjects(WebDAVMountPrefix(id))`. Return:

```json
{"error":{"code":"mount_not_empty","message":"empty the WebDAV mount before deleting its credential"}}
```

with HTTP 409 when the count is non-zero. Revocation and rotation do not alter files. After creating the first WebDAV credential, rerun deferred namespace migration; successful creation remains a 201 response.

- **Step 6: Run R2 and HTTP API tests**

Run: `go test ./internal/modules/r2 ./internal/platform/httpapi -count=1`

Expected: PASS.

- **Step 7: Commit the mount-aware API**

```bash
git add internal/modules/r2/file_manager.go internal/modules/r2/file_manager_test.go internal/platform/httpapi/files.go internal/platform/httpapi/files_test.go internal/platform/httpapi/api.go internal/platform/httpapi/api_test.go
git commit -m "feat: expose WebDAV mounts in file manager API"
```

### Task 6: Render mounts as the file-manager root

**Files:**
- Modify: `web/src/types.ts`
- Modify: `web/src/App.tsx`
- Modify: `web/src/pages/FilesPage.tsx`
- Modify: `web/src/components/FileManagerDialogs.tsx`
- Modify: `web/src/styles/pages.css`
- Modify: `web/src/styles/responsive.css`

- **Step 1: Update shared types and hash routing**

Change `FileEntry.kind` to `"mount" | "directory" | "file"` and add `mount_id?: string` plus `disabled?: boolean`. Add `mount_id?: string` and `mount_name?: string` to `FileDirectoryList`.

Use this route state:

```ts
interface RouteState {
  page: PageID;
  fileMountID: string;
  filePath: string;
}
```

Parse `#files?mount=<credential-id>&path=<relative-path>`. `onNavigate(mountID, path)` writes both values; returning to the virtual root writes `#files`.

- **Step 2: Scope all requests and content URLs**

Every directory request includes `mount_id` when inside a mount. Upload, preview, download, create-directory, move, rename, and delete requests include the current entry's `mount_id`. Change:

```ts
export function contentURL(entry: FileEntry, mode: "preview" | "download") {
  const query = new URLSearchParams({ mount_id: entry.mount_id ?? "", key: entry.key, mode });
  return `/api/v1/files/content?${query}`;
}
```

Pass `mountID` into `MoveDialog` directory loading so the picker never leaves the current mount.

- **Step 3: Replace the root UI**

Remove imports and JSX for `Files`, `FolderTree`, and `.file-summary-band`. At the virtual root:

- show only Refresh in the page header;
- render mount entries in the existing details table with folder icons;
- clicking a mount navigates to `{mountID: entry.mount_id, path: ""}`;
- suppress context menus and the ellipsis button for mount rows;
- label the type `WebDAV 挂载点` and show `已撤销` without blocking admin navigation.

Inside a mount, show New Folder and Upload. Build breadcrumbs as `根目录 > <mount_name> > ...`; clicking `根目录` clears the mount ID, and Up from the mount root returns to the virtual root.

- **Step 4: Remove obsolete CSS and verify responsive constraints**

Delete `.file-summary-band` rules created by the previous implementation. Add only the mount-row muted state and any toolbar wrapping required at 390px. Keep table column widths, action-cell dimensions, and button heights stable.

- **Step 5: Build the web app**

Run: `npm run build` in `web/`.

Expected: TypeScript and Vite build succeed; no unused icon imports or route-state type errors.

- **Step 6: Commit the frontend**

```bash
git add web/src/types.ts web/src/App.tsx web/src/pages/FilesPage.tsx web/src/components/FileManagerDialogs.tsx web/src/styles/pages.css web/src/styles/responsive.css
git commit -m "feat: show WebDAV mounts as file manager folders"
```

### Task 7: Full regression and browser verification

**Files:**
- Modify only files implicated by failures found in this task.

- **Step 1: Run formatting and whitespace checks**

Run: `gofmt -w internal/modules/r2/webdav_mounts.go internal/modules/r2/webdav_mounts_test.go internal/protocol/webdav/scoped_objects.go internal/protocol/webdav/handler.go internal/protocol/webdav/handler_test.go internal/protocol/s3/handler.go internal/protocol/s3/multipart.go internal/protocol/s3/handler_test.go internal/protocol/s3/multipart_test.go internal/platform/httpapi/files.go internal/platform/httpapi/files_test.go internal/platform/httpapi/api.go internal/platform/httpapi/api_test.go internal/app/server.go`

Run: `git diff --check`

Expected: no whitespace errors.

- **Step 2: Run the full backend suite**

Run: `go test ./... -count=1`

Expected: PASS, including WebDAV empty-root/RaiDrive, S3 multipart, jobs, migration, and HTTP API tests.

- **Step 3: Run the production frontend build**

Run: `npm run build` in `web/`.

Expected: PASS. Existing bundle-size warnings are acceptable; new TypeScript errors are not.

- **Step 4: Verify the upgraded data shape in an isolated copy**

Start the app against a copied database containing the two credentials and legacy files. Confirm the file-manager root contains exactly `gamesync` and `test`, `gamesync` contains `GameSync/` and `claude.png`, and `test` is empty. Confirm both WebDAV credentials can mount an empty or populated root independently.

- **Step 5: Verify desktop and mobile UI**

At 1280x720 and 390x844:

- root summary band is absent;
- mount names, toolbar, breadcrumb, columns, and menu buttons do not overlap;
- root hides Upload and New Folder;
- `gamesync` shows both commands and legacy content;
- browser back/forward and refresh preserve mount plus relative path;
- browser console contains no errors.

- **Step 6: Review the final diff**

Run: `git status --short`, `git diff --stat`, and `git diff --check`.

Expected: only the spec, plan, implementation, and focused tests are changed; no database copy, temporary config, password, or built `dist/` artifact is tracked.
