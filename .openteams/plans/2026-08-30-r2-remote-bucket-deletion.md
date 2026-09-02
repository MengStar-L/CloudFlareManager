# R2 Remote Bucket Deletion Implementation Plan

**Goal:** Add explicit remote R2 bucket deletion and a safe “一键清空并删除桶” workflow, with persistent progress, fail-closed identity checks, local lifecycle fencing, and stable Chinese failure messages.

**Architecture:** Keep local deregistration and remote destruction as separate commands. A protected HTTP endpoint validates the administrator password and exact bucket-name confirmation, snapshots the remote bucket identity, fences a managed bucket with a persistent lifecycle state, and atomically enqueues one resource-scoped background job. The worker repeatedly revalidates identity, settles local writes, prefers S3 batch deletion and multipart abort, falls back to Cloudflare REST object deletion when possible, deletes the empty bucket, and removes local metadata only after remote absence is verified. Job payload and progress make retries resumable; stable error codes drive Chinese UI copy.

**Tech Stack:** Go 1.26, SQLite, Cloudflare REST API, AWS SDK for Go v2 S3 client, `net/http`/`httptest`, React 19, TypeScript 5.7, Vite 6, Lucide React

---

## File Map

- Add `internal/platform/database/migrations/007_r2_remote_bucket_deletion.sql`: persistent bucket lifecycle, deletion-job linkage, job resource identity, error code, retry parent, and active-job uniqueness.
- Modify `internal/platform/database/database_test.go`: migration/backfill/constraint/index coverage.
- Add `internal/platform/jobs/errors.go`: coded transient/permanent handler failures.
- Modify `internal/platform/jobs/store.go`: resource-scoped enqueue, error-code persistence, parent jobs, and permanent failure.
- Modify `internal/platform/jobs/store_test.go`: active-resource deduplication and terminal-job replacement.
- Modify `internal/platform/jobs/runner.go`; add `internal/platform/jobs/runner_test.go`: classify permanent versus retryable handler failures.
- Add `internal/platform/accounts/cloudflare_error.go`: structured Cloudflare API error and response helpers.
- Modify `internal/platform/accounts/remote.go` and `internal/platform/accounts/remote_test.go`: jurisdiction-aware bucket identity, object listing/deletion, and bucket deletion.
- Modify `internal/modules/r2/aws_backend.go` and `internal/modules/r2/aws_backend_test.go`: S3 batch object deletion and remote multipart listing.
- Add `internal/modules/r2/bucket_lifecycle.go` and `internal/modules/r2/bucket_lifecycle_test.go`: lifecycle transitions, write settlement queries, deletion maintenance lock, and transactional finalization.
- Modify `internal/modules/r2/store.go`, `write_intents.go`, `write_intents_test.go`, `multipart_store.go`, `multipart.go`, `multipart_test.go`, `maintenance.go`, `maintenance_store.go`, `maintenance_test.go`, `cleanup.go`, and `usage_store.go`: direct lifecycle gates at every read/write/multipart/maintenance admission point.
- Modify `internal/modules/r2/errors.go`: stable lifecycle errors.
- Modify `internal/protocol/s3/handler.go`, `internal/protocol/s3/multipart.go`, and tests: map fenced buckets to S3 `ServiceUnavailable`.
- Modify `internal/protocol/webdav/handler.go` and tests: map fenced buckets to HTTP 503.
- Modify `internal/platform/httpapi/files.go` and `files_test.go`: stable `bucket_deleting` JSON response for file API reads and writes.
- Add `internal/modules/r2/bucket_deletion.go`, `bucket_deletion_jobs.go`, and `bucket_deletion_jobs_test.go`: remote purge interfaces and resumable deletion state machine.
- Modify `internal/app/server.go`: register the deletion job handler with real Cloudflare and S3 dependencies.
- Add `internal/platform/httpapi/r2_bucket_deletion.go` and `r2_bucket_deletion_test.go`; modify `internal/platform/httpapi/api.go` and `api_test.go`: request validation, admin confirmation, remote identity snapshot, job enqueue, view state, audit events, and Chinese errors.
- Modify `web/src/api.ts` and `web/src/types.ts`: retain API error codes and model lifecycle/deletion job fields.
- Add `web/src/components/BucketDeleteDialog.tsx`: dedicated destructive-mode dialog with exact-name and password confirmation.
- Modify `web/src/pages/StoragePage.tsx`: separate unlink/delete actions, progress polling, retries, disabled jurisdictions, and Chinese error handling.
- Modify `web/src/components/UI.tsx` and `web/src/pages/ActivityPage.tsx`: lifecycle status styling and readable deletion-job activity.
- Modify `web/src/styles/components.css` and `web/src/styles/responsive.css`: compact dialog/progress/responsive styles.
- Modify `web/src/styles/pages.css`: stable bucket-row deletion status and retry layout.
- Modify `docs/api.md`: asynchronous deletion endpoint, modes, confirmation rules, and stable errors.
- Keep `.openteams/specs/2026-08-30-r2-remote-bucket-deletion-design.html` and this plan as the reviewed implementation record.

## Stable Contracts

Use these names consistently across database, Go, JSON, and TypeScript:

```go
type BucketLifecycleState string

const (
	BucketActive       BucketLifecycleState = "active"
	BucketDeleting     BucketLifecycleState = "deleting"
	BucketDeleteFailed BucketLifecycleState = "delete_failed"
)

type BucketDeletionMode string

const (
	BucketDeletionEmptyOnly      BucketDeletionMode = "empty_only"
	BucketDeletionEmptyAndDelete BucketDeletionMode = "empty_and_delete"
)

const BucketDeletionJobType = "r2.bucket.delete-remote"
```

The API accepts:

```json
{
  "account_id": "manager-account-id",
  "jurisdiction": "default",
  "mode": "empty_and_delete",
  "confirmation_name": "exact-bucket-name",
  "admin_password": "current-admin-password"
}
```

The endpoint is `POST /api/v1/r2/remote-buckets/{bucket_name}/deletions`. It returns `202` with `{ "job": ... }`; an already active resource job returns the same job instead of scheduling a duplicate.

Stable deletion error codes and user-facing Chinese messages:

| Code | Chinese message |
| --- | --- |
| `bucket_not_empty` | 存储桶内仍有文件。请先删除桶内所有文件，或选择“一键清空并删除桶”。 |
| `bucket_busy` | 存储桶仍有上传或写入任务，请稍后重试。 |
| `bucket_deleting` | 存储桶正在删除，当前不能读取、写入或执行维护操作。 |
| `bucket_locked` | Cloudflare 暂时锁定了该存储桶，请稍后重试。 |
| `permission_denied` | Cloudflare Token 没有删除该存储桶所需的 Workers R2 Storage Write 权限。 |
| `s3_credentials_required` | 桶内存在无法通过 REST 清理的分片上传，请为该账号配置 S3 凭据后重试。 |
| `external_writes_detected` | 删除期间检测到新的外部写入，任务已停止。请停止其他客户端写入后重试。 |
| `bucket_identity_changed` | 同名存储桶的身份已变化，任务已停止以避免误删。 |
| `bucket_identity_unverifiable` | 无法确认存储桶身份，任务已停止以避免误删。 |
| `unsupported_jurisdiction` | 当前版本仅支持删除默认管辖区的 R2 存储桶。 |
| `partial_delete_failed` | 部分文件删除失败，未删除存储桶；可在排除原因后重试。 |
| `rate_limited` | Cloudflare 请求过于频繁，任务将自动重试。 |
| `cloudflare_unavailable` | Cloudflare 服务暂时不可用，任务将自动重试。 |
| `local_finalize_failed` | 远端存储桶已删除，但本地登记清理失败；请重试以完成本地收尾。 |

### Task 1: Persist Job Identity and Bucket Lifecycle

**Files:**
- Add: `internal/platform/database/migrations/007_r2_remote_bucket_deletion.sql`
- Modify: `internal/platform/database/database_test.go`

**Step 1: Write the failing migration test**

Add `TestRemoteBucketDeletionMigrationAddsJobIdentityAndBucketLifecycle`. Create a database migrated only through `006_r2_transactional_placement.sql`, insert one existing bucket and two terminal jobs, reopen it with `database.Open`, then assert:

```go
var lifecycle, deletionJobID string
if err := db.QueryRow(`SELECT lifecycle_state, COALESCE(deletion_job_id, '')
	FROM r2_physical_buckets WHERE id = 'bucket'`).Scan(&lifecycle, &deletionJobID); err != nil {
	t.Fatal(err)
}
if lifecycle != "active" || deletionJobID != "" {
	t.Fatalf("lifecycle=%q deletion_job_id=%q", lifecycle, deletionJobID)
}
```

Also insert two active jobs with the same `(type, resource_key)` and expect the second insert to fail; insert a terminal job with the same key and expect success; attempt `lifecycle_state='unknown'` and expect the CHECK constraint to fail.

**Step 2: Run the test and verify failure**

```powershell
go test ./internal/platform/database -run TestRemoteBucketDeletionMigrationAddsJobIdentityAndBucketLifecycle -count=1
```

Expected: FAIL because the migration and columns do not exist.

**Step 3: Add migration 007**

```sql
ALTER TABLE jobs ADD COLUMN resource_key TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN error_code TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN parent_job_id TEXT REFERENCES jobs(id);

CREATE UNIQUE INDEX jobs_active_resource_idx
ON jobs(type, resource_key)
WHERE resource_key <> '' AND status IN ('pending', 'running');

ALTER TABLE r2_physical_buckets ADD COLUMN lifecycle_state TEXT NOT NULL DEFAULT 'active'
CHECK(lifecycle_state IN ('active', 'deleting', 'delete_failed'));
ALTER TABLE r2_physical_buckets ADD COLUMN deletion_job_id TEXT REFERENCES jobs(id);

CREATE INDEX r2_physical_buckets_lifecycle_idx
ON r2_physical_buckets(lifecycle_state, deletion_job_id);
```

Do not change `writable`; lifecycle is an independent safety state.

**Step 4: Run the migration tests**

```powershell
gofmt -w internal/platform/database/database_test.go
go test ./internal/platform/database -count=1
```

Expected: PASS.

**Step 5: Commit the migration**

```powershell
git add internal/platform/database/migrations/007_r2_remote_bucket_deletion.sql internal/platform/database/database_test.go
git commit -m "Persist R2 bucket deletion lifecycle"
```

### Task 2: Add Resource-Scoped Jobs and Stable Failure Codes

**Files:**
- Add: `internal/platform/jobs/errors.go`
- Modify: `internal/platform/jobs/store.go`
- Modify: `internal/platform/jobs/store_test.go`
- Modify: `internal/platform/jobs/runner.go`
- Add: `internal/platform/jobs/runner_test.go`

**Step 1: Write failing store tests**

Add:

```go
func TestEnqueueUniqueDeduplicatesActiveResource(t *testing.T)
func TestEnqueueUniqueAllowsResourceAfterTerminalJob(t *testing.T)
func TestFailPermanentPersistsCodeWithoutRetry(t *testing.T)
```

The first test launches two goroutines against the same resource key, then asserts both calls resolve to one job ID and exactly one reports `created=true`. The second completes the first job and expects a new ID. The third claims a job, calls `FailPermanent`, and asserts `status=failed`, `error_code` is retained, and `lease_until=nil`.

**Step 2: Run and verify failure**

```powershell
go test ./internal/platform/jobs -run 'TestEnqueueUnique|TestFailPermanent' -count=1
```

Expected: compilation fails for missing APIs and fields.

**Step 3: Extend the job model and store**

Add fields:

```go
ResourceKey string `json:"resource_key,omitempty"`
ErrorCode   string `json:"error_code,omitempty"`
ParentJobID string `json:"parent_job_id,omitempty"`
```

Keep `Enqueue` as a wrapper over an internal insert with an empty resource key. Add:

```go
func (s *Store) EnqueueUnique(
	ctx context.Context,
	jobType, resourceKey, parentJobID string,
	payload any,
	maxAttempts int,
) (job Job, created bool, err error)
```

Use a transaction and the partial unique index. If the insert loses the race, select the pending/running row for the same type/resource and return it with `created=false`. Do not rely on a pre-insert SELECT.

Change retry failure persistence to:

```go
func (s *Store) Fail(ctx context.Context, id, code, message string, retryAt time.Time) error
func (s *Store) FailPermanent(ctx context.Context, id, code, message string) error
```

Update every job SELECT and `scanJob`, clear both `error` and `error_code` on `Complete`, and update existing call sites to pass an empty code.

**Step 4: Add classified handler errors**

```go
type HandlerError struct {
	Code      string
	Permanent bool
	Err       error
}

func (e *HandlerError) Error() string { return e.Err.Error() }
func (e *HandlerError) Unwrap() error { return e.Err }

func NewFailure(code string, err error, permanent bool) error {
	return &HandlerError{Code: code, Err: err, Permanent: permanent}
}
```

Add a classifier that defaults ordinary errors to transient with an empty code.

**Step 5: Write and implement runner classification tests**

Add `TestRunnerPermanentlyFailsClassifiedError` and `TestRunnerRetriesClassifiedTransientError`. A permanent failure must consume one attempt and become terminal immediately. A transient coded failure must return to pending with its code and retry timestamp.

Update `Runner.runOne` so a missing handler is permanent, a classified permanent error calls `FailPermanent`, and other failures call `Fail` with the retry policy.

**Step 6: Format and run jobs tests**

```powershell
gofmt -w internal/platform/jobs/errors.go internal/platform/jobs/store.go internal/platform/jobs/store_test.go internal/platform/jobs/runner.go internal/platform/jobs/runner_test.go
go test ./internal/platform/jobs -count=1
```

Expected: PASS.

**Step 7: Commit job primitives**

```powershell
git add internal/platform/jobs
git commit -m "Add resource scoped background jobs"
```

### Task 3: Add Structured Cloudflare R2 Deletion Operations

**Files:**
- Add: `internal/platform/accounts/cloudflare_error.go`
- Modify: `internal/platform/accounts/remote.go`
- Modify: `internal/platform/accounts/remote_test.go`

**Step 1: Write failing remote-client tests**

Add these `httptest.Server` cases:

```go
func TestRemoteClientGetsBucketIdentity(t *testing.T)
func TestRemoteClientListsObjectsAcrossPages(t *testing.T)
func TestRemoteClientDeletesObjectWithLiteralSlashes(t *testing.T)
func TestRemoteClientDeletesBucket(t *testing.T)
func TestRemoteClientClassifiesCloudflareErrors(t *testing.T)
func TestRemoteClientRejectsSuccessFalse(t *testing.T)
func TestRemoteClientHandlesNonJSONError(t *testing.T)
```

For the slash case, require the server to observe `/objects/folder/nested/file.txt`, not `%2F`. For all deletion calls, assert `cf-r2-jurisdiction: default` and bearer authentication. Cover 401/403, 404, 409 code 10008, 429, 5xx, and `200` with `success:false`.

**Step 2: Run and verify failure**

```powershell
go test ./internal/platform/accounts -run 'TestRemoteClient(Get|List|Delete|Classifies|Rejects|Handles)' -count=1
```

Expected: compilation fails because the new methods and typed errors do not exist.

**Step 3: Add a structured Cloudflare error**

```go
type CloudflareAPIError struct {
	Operation  string
	StatusCode int
	Code       int
	Message    string
}

func (e *CloudflareAPIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("%s: cloudflare code %d: %s", e.Operation, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: cloudflare HTTP %d: %s", e.Operation, e.StatusCode, e.Message)
}
```

Centralize bounded body reads, envelope decoding, and `success:false` handling so destructive methods never parse errors by string matching.

**Step 4: Add jurisdiction-aware identity and object APIs**

Extend `RemoteBucket`:

```go
type RemoteBucket struct {
	Name         string `json:"name"`
	CreationDate string `json:"creation_date,omitempty"`
	Jurisdiction string `json:"jurisdiction"`
	Location     string `json:"location,omitempty"`
	StorageClass string `json:"storage_class,omitempty"`
}
```

Add:

```go
type RemoteObject struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

type RemoteObjectPage struct {
	Objects   []RemoteObject `json:"objects"`
	Cursor    string         `json:"cursor,omitempty"`
	Truncated bool           `json:"truncated"`
}

func (c RemoteClient) GetR2Bucket(ctx context.Context, accountID, token, jurisdiction, bucket string) (RemoteBucket, error)
func (c RemoteClient) ListR2Objects(ctx context.Context, accountID, token, jurisdiction, bucket, cursor string, limit int) (RemoteObjectPage, error)
func (c RemoteClient) DeleteR2Object(ctx context.Context, accountID, token, jurisdiction, bucket, key string) error
func (c RemoteClient) DeleteR2Bucket(ctx context.Context, accountID, token, jurisdiction, bucket string) error
```

Build object paths by escaping each key segment separately and rejoining with `/`. Treat a missing/unparseable creation time as an identity error in the deletion layer, not as a best-effort match.

For this release, keep the existing default bucket listing behavior and set `Jurisdiction="default"`. The HTTP layer rejects destructive requests for other jurisdictions even if later listings expose them.

**Step 5: Run remote-client tests**

```powershell
gofmt -w internal/platform/accounts/cloudflare_error.go internal/platform/accounts/remote.go internal/platform/accounts/remote_test.go
go test ./internal/platform/accounts -count=1
```

Expected: PASS.

**Step 6: Commit Cloudflare operations**

```powershell
git add internal/platform/accounts/cloudflare_error.go internal/platform/accounts/remote.go internal/platform/accounts/remote_test.go
git commit -m "Add Cloudflare R2 deletion operations"
```

### Task 4: Add S3 Batch Purge and Multipart Discovery

**Files:**
- Modify: `internal/modules/r2/aws_backend.go`
- Modify: `internal/modules/r2/aws_backend_test.go`
- Add: `internal/modules/r2/bucket_deletion.go`

**Step 1: Write failing AWS backend tests**

Add:

```go
func TestAWSBackendDeletesObjectBatch(t *testing.T)
func TestAWSBackendReportsPartialBatchDeleteErrors(t *testing.T)
func TestAWSBackendListsRemoteMultipart(t *testing.T)
```

The fake S3 endpoint must assert at most 1000 keys, decode the XML `DeleteObjects` request, return an HTTP-200 body with one `<Error>`, and verify that partial errors are surfaced. Multipart listing must preserve both key and upload-ID continuation markers.

**Step 2: Run and verify failure**

```powershell
go test ./internal/modules/r2 -run 'TestAWSBackend(DeletesObjectBatch|ReportsPartialBatchDeleteErrors|ListsRemoteMultipart)' -count=1
```

Expected: compilation fails for missing APIs.

**Step 3: Define the narrow deletion backend interface**

Do not expand the shared `Backend` interface, which would break unrelated test doubles.

```go
type RemoteMultipart struct {
	Key      string
	UploadID string
}

type RemoteMultipartPage struct {
	Uploads           []RemoteMultipart
	NextKeyMarker     string
	NextUploadIDMarker string
	Truncated         bool
}

type BucketClearBackend interface {
	ListRemote(ctx context.Context, target Target, after string, limit int32) (RemoteObjectList, error)
	DeleteRemoteBatch(ctx context.Context, target Target, keys []string) (int, error)
	ListRemoteMultipart(ctx context.Context, target Target, keyMarker, uploadIDMarker string, limit int32) (RemoteMultipartPage, error)
	AbortMultipart(ctx context.Context, target Target, key, uploadID string) error
}
```

**Step 4: Implement AWS methods**

`DeleteRemoteBatch` validates `1 <= len(keys) <= 1000`, calls `DeleteObjects`, checks `output.Errors` even on HTTP 200, and reports keys/codes without logging secrets. `ListRemoteMultipart` calls `ListMultipartUploads` and returns continuation markers. Reuse the existing `AbortMultipart` method.

**Step 5: Run focused R2 tests**

```powershell
gofmt -w internal/modules/r2/aws_backend.go internal/modules/r2/aws_backend_test.go internal/modules/r2/bucket_deletion.go
go test ./internal/modules/r2 -run 'TestAWSBackend' -count=1
```

Expected: PASS.

**Step 6: Commit the purge backend**

```powershell
git add internal/modules/r2/aws_backend.go internal/modules/r2/aws_backend_test.go internal/modules/r2/bucket_deletion.go
git commit -m "Add efficient R2 bucket purge primitives"
```

### Task 5: Add Persistent Bucket Lifecycle and Atomic Finalization

**Files:**
- Add: `internal/modules/r2/bucket_lifecycle.go`
- Add: `internal/modules/r2/bucket_lifecycle_test.go`
- Modify: `internal/modules/r2/store.go`
- Modify: `internal/modules/r2/errors.go`

**Step 1: Write failing lifecycle tests**

Add:

```go
func TestBeginBucketDeletionFencesNewWork(t *testing.T)
func TestBeginBucketDeletionCountsPreviousObjectIntent(t *testing.T)
func TestDeletionRetryRebindsOnlyFromParentFailedJob(t *testing.T)
func TestRestoreBucketActivePreservesWritable(t *testing.T)
func TestFinalizeDeletedBucketRemovesEveryReferenceAtomically(t *testing.T)
```

The previous-object test creates an overwrite whose `target_bucket_id` is a different bucket while `previous_object_id` still maps to the bucket being deleted. It must count as blocking activity. The finalization test inserts rows in objects, write intents, multipart uploads/parts/reservations, cleanup queue, findings, rules, maintenance lock, and expired WebDAV locks, then asserts the single transaction removes all bucket references but leaves active WebDAV locks to expire naturally.

**Step 2: Run and verify failure**

```powershell
go test ./internal/modules/r2 -run 'Test(BeginBucketDeletion|DeletionRetry|RestoreBucketActive|FinalizeDeletedBucket)' -count=1
```

Expected: compilation fails for missing lifecycle APIs.

**Step 3: Extend `PhysicalBucket` and bucket scans**

Add `LifecycleState BucketLifecycleState` and `DeletionJobID string`. Update the shared `bucketSelect`, `scanBucket`, inserts, and list/get tests. New buckets must return `active` without changing `Writable` semantics.

Add errors:

```go
var (
	ErrBucketDeleting = errors.New("bucket deletion is in progress")
	ErrBucketInUse    = errors.New("bucket still has active work")
)
```

**Step 4: Implement lifecycle transitions**

```go
func (s *Store) BeginBucketDeletion(ctx context.Context, bucketID, jobID, parentJobID string) (PhysicalBucket, error)
func (s *Store) MarkBucketDeletionFailed(ctx context.Context, bucketID, jobID string) error
func (s *Store) RestoreBucketActive(ctx context.Context, bucketID, jobID string) error
func (s *Store) HasDeletionBlockingActivity(ctx context.Context, bucketID string) (bool, error)
func (s *Store) AcquireBucketDeletionMaintenance(ctx context.Context, bucketID, jobID string) error
func (s *Store) FinalizeDeletedBucket(ctx context.Context, bucketID, jobID string) error
```

`BeginBucketDeletion` permits `active -> deleting`, or `delete_failed -> deleting` only when the new job names the failed job as parent. It refuses an unrelated maintenance lock. `RestoreBucketActive` updates only the bucket still owned by this job and never modifies `writable`.

Blocking activity must include both target and previous-object ownership:

```sql
SELECT EXISTS(
  SELECT 1 FROM r2_write_intents AS wi
  WHERE wi.target_bucket_id = ?
     OR EXISTS (
       SELECT 1 FROM r2_objects AS o
       WHERE o.object_id = wi.previous_object_id
         AND o.physical_bucket_id = ?
     )
) OR EXISTS(
  SELECT 1 FROM r2_multipart_uploads WHERE physical_bucket_id = ?
)
```

`FinalizeDeletedBucket` checks `lifecycle_state='deleting' AND deletion_job_id=?` and performs dependency-ordered cleanup in one transaction. Do not call the existing `DeleteBucket`; several references do not cascade.

**Step 5: Run lifecycle tests**

```powershell
gofmt -w internal/modules/r2/bucket_lifecycle.go internal/modules/r2/bucket_lifecycle_test.go internal/modules/r2/store.go internal/modules/r2/errors.go
go test ./internal/modules/r2 -run 'Test(BeginBucketDeletion|DeletionRetry|RestoreBucketActive|FinalizeDeletedBucket)' -count=1
```

Expected: PASS.

**Step 6: Commit lifecycle storage**

```powershell
git add internal/modules/r2/bucket_lifecycle.go internal/modules/r2/bucket_lifecycle_test.go internal/modules/r2/store.go internal/modules/r2/errors.go
git commit -m "Fence managed buckets during remote deletion"
```

### Task 6: Enforce Lifecycle at Every Protocol and Maintenance Admission Point

**Files:**
- Modify: `internal/modules/r2/write_intents.go`
- Modify: `internal/modules/r2/write_intents_test.go`
- Modify: `internal/modules/r2/multipart_store.go`
- Modify: `internal/modules/r2/multipart.go`
- Modify: `internal/modules/r2/multipart_test.go`
- Modify: `internal/modules/r2/service.go`
- Modify: `internal/modules/r2/service_test.go`
- Modify: `internal/modules/r2/maintenance.go`
- Modify: `internal/modules/r2/maintenance_store.go`
- Modify: `internal/modules/r2/maintenance_test.go`
- Modify: `internal/modules/r2/cleanup.go`
- Modify: `internal/modules/r2/usage_store.go`
- Modify: `internal/protocol/s3/handler.go`
- Modify: `internal/protocol/s3/handler_test.go`
- Modify: `internal/protocol/s3/multipart.go`
- Modify: `internal/protocol/s3/multipart_test.go`
- Modify: `internal/protocol/webdav/handler.go`
- Modify: `internal/protocol/webdav/handler_test.go`
- Modify: `internal/platform/httpapi/files.go`
- Modify: `internal/platform/httpapi/files_test.go`

**Step 1: Write failing R2 admission tests**

Add cases proving:

- placement never selects a non-active bucket;
- overwrite and logical delete reject when the existing mapping is non-active, even if another active target exists;
- multipart upload-part and complete reject non-active buckets, while abort/recovery remains allowed;
- `Stat`/`Get` return `ErrBucketDeleting`, but logical list retains the indexed item;
- adopt, orphan scan, cleanup, expiry, and rebalance reject or skip non-active buckets;
- rebalance validates both source and target lifecycle.

Use exact test names:

```go
func TestBeginWriteRejectsDeletingPreviousBucket(t *testing.T)
func TestBeginDeleteRejectsDeletingBucket(t *testing.T)
func TestMultipartRejectsNewPartsButAllowsAbortWhileDeleting(t *testing.T)
func TestDeletingBucketReadsUnavailableButListRetainsIndex(t *testing.T)
func TestMaintenanceRejectsDeletingBucket(t *testing.T)
func TestRebalanceRejectsDeletingSourceOrTarget(t *testing.T)
```

**Step 2: Run and verify failure**

```powershell
go test ./internal/modules/r2 -run 'Test(BeginWriteRejectsDeleting|BeginDeleteRejectsDeleting|MultipartRejects|DeletingBucketReads|MaintenanceRejects|RebalanceRejects)' -count=1
```

Expected: FAIL because lifecycle is not checked at these transaction boundaries.

**Step 3: Add direct database predicates**

Apply lifecycle checks inside the same transactions that create or advance work:

- `selectWriteBucket`: `lifecycle_state='active'`.
- `BeginWrite`: the previous object bucket must be active.
- `BeginDeleteWriteConditional`: object bucket must be active.
- multipart part preparation and begin-complete: upload bucket must be active.
- normal `AcquireBucketMaintenance`: lifecycle must be active.
- `AdoptObject` and `FinishBucketScan`: update only active buckets.

Do not put a global lifecycle rejection inside `Service.target`; deletion settlement and interrupted-write recovery still need credentials for fenced buckets.

**Step 4: Gate reads and maintenance**

Before remote ETag repair or object reads, call `EnsureBucketActive`. Keep logical list output available so the UI and recovery tools do not lose indexed objects. Skip non-active buckets in expiry/periodic cleanup; explicit deletion settlement owns them. Require active source and target in rebalance.

**Step 5: Add protocol mapping tests and implementation**

Assert S3 returns 503 `ServiceUnavailable`, WebDAV returns HTTP 503, and file API returns:

```json
{
  "error": {
    "code": "bucket_deleting",
    "message": "存储桶正在删除，当前不能读取、写入或执行维护操作。"
  }
}
```

Apply mappings in each protocol’s existing centralized error switch.

**Step 6: Run focused module/protocol tests**

```powershell
gofmt -w internal/modules/r2 internal/protocol/s3 internal/protocol/webdav internal/platform/httpapi/files.go internal/platform/httpapi/files_test.go
go test ./internal/modules/r2 ./internal/protocol/s3 ./internal/protocol/webdav ./internal/platform/httpapi -count=1
```

Expected: PASS.

**Step 7: Commit lifecycle enforcement**

```powershell
git add internal/modules/r2 internal/protocol/s3 internal/protocol/webdav internal/platform/httpapi/files.go internal/platform/httpapi/files_test.go
git commit -m "Enforce R2 deletion lifecycle across protocols"
```

### Task 7: Implement the Resumable Remote Deletion State Machine

**Files:**
- Modify: `internal/modules/r2/bucket_deletion.go`
- Add: `internal/modules/r2/bucket_deletion_jobs.go`
- Add: `internal/modules/r2/bucket_deletion_jobs_test.go`

**Step 1: Write failing state-machine tests**

Use in-memory fake Cloudflare and S3 backends with an operation log. Add:

```go
func TestBucketDeletionEmptyOnlyRejectsNonEmptyWithoutMutation(t *testing.T)
func TestBucketDeletionEmptyOnlyDeletesEmptyBucket(t *testing.T)
func TestBucketDeletionEmptyAndDeleteResumesAfterPartialClear(t *testing.T)
func TestBucketDeletionAbortsRemoteMultipart(t *testing.T)
func TestBucketDeletionFallsBackToRESTObjects(t *testing.T)
func TestBucketDeletionRequiresS3ForMultipart(t *testing.T)
func TestBucketDeletionTreatsVerifiedRemoteMissingAsSuccess(t *testing.T)
func TestBucketDeletionStopsWhenIdentityChanges(t *testing.T)
func TestBucketDeletionDetectsExternalWrites(t *testing.T)
func TestBucketDeletionRechecksAfterDeleteTimeout(t *testing.T)
func TestBucketDeletionKeepsLocalStateWhenFinalizeFails(t *testing.T)
```

Assert operation ordering: lifecycle fence precedes any remote mutation; identity GET precedes every destructive page; local finalization occurs only after a successful DELETE or verified 404; failures after mutation leave `delete_failed`; non-mutating permanent failures restore the prior local state.

**Step 2: Run and verify failure**

```powershell
go test ./internal/modules/r2 -run 'TestBucketDeletion' -count=1
```

Expected: compilation fails for missing handler and payload.

**Step 3: Define payload, stages, and dependencies**

```go
type BucketDeletionPayload struct {
	AccountID             string               `json:"account_id"`
	CloudflareAccountID   string               `json:"cloudflare_account_id"`
	BucketName            string               `json:"bucket_name"`
	Jurisdiction          string               `json:"jurisdiction"`
	ExpectedCreationDate  string               `json:"expected_creation_date"`
	LocalBucketID         string               `json:"local_bucket_id,omitempty"`
	Mode                  BucketDeletionMode   `json:"mode"`
	Stage                 string               `json:"stage"`
	ParentJobID           string               `json:"parent_job_id,omitempty"`
	RemoteMissingAtEnqueue bool                 `json:"remote_missing_at_enqueue"`
	RemoteMutated         bool                 `json:"remote_mutated"`
	DeletedObjects        int64                `json:"deleted_objects"`
	AbortedMultipart      int64                `json:"aborted_multipart"`
	DeleteRounds          int                  `json:"delete_rounds"`
}

type BucketDeletionRemote interface {
	GetR2Bucket(context.Context, string, string, string, string) (accounts.RemoteBucket, error)
	ListR2Objects(context.Context, string, string, string, string, string, int) (accounts.RemoteObjectPage, error)
	DeleteR2Object(context.Context, string, string, string, string, string) error
	DeleteR2Bucket(context.Context, string, string, string, string) error
}

type BucketDeletionJobs struct {
	Service Service
	Jobs    *jobs.Store
	Remote  BucketDeletionRemote
	Audit   *audit.Store
}
```

Fetch the account token at execution time through `Service.Accounts`; never persist it in the payload.

**Step 4: Implement fail-closed identity checks**

Normalize the baseline and current creation timestamp to UTC before equality. Account ID, jurisdiction, bucket name, and creation time are all identity fields. Missing or unparseable creation time returns permanent `bucket_identity_unverifiable`; mismatch returns permanent `bucket_identity_changed`.

Call identity GET:

1. before fencing/mutation;
2. before each S3 or REST delete page;
3. immediately before Delete Bucket;
4. after an ambiguous/timeout Delete Bucket response, before any retry.

Cloudflare has no conditional bucket deletion. Document and surface the residual rule: do not externally recreate the same bucket name while the task is active.

**Step 5: Implement local settlement and maintenance ownership**

After `BeginBucketDeletion`, abort existing local multipart uploads and resolve existing write intents using the existing recovery paths. Poll the database only through bounded worker iterations; return retryable `bucket_busy` while active work remains. Acquire `AcquireBucketDeletionMaintenance` only after activity reaches zero.

**Step 6: Implement clear and delete modes**

For `empty_only`, list visible objects without mutation for an early Chinese failure, then let Delete Bucket be the authoritative empty check. If objects exist or Cloudflare returns BucketNotEmpty (including hidden multipart state), restore active state for an unmutated job and fail permanently with `bucket_not_empty`.

For `empty_and_delete`:

- persist `RemoteMutated=true` before the first destructive call;
- prefer S3 pages of up to 1000 objects and inspect partial delete errors;
- abort all remote multipart uploads and repeat listing until empty;
- when S3 credentials are unavailable, delete ordinary objects one by one through REST;
- if multipart uploads prevent final deletion without S3 credentials, fail permanently with `s3_credentials_required`;
- after reaching empty, re-list from the first page to detect external writes;
- classify reappearing keys as `external_writes_detected` rather than looping forever.

Persist payload and progress after every batch. Keep progress below `0.95` until remote absence is verified and local finalization succeeds.

**Step 7: Map remote failures**

Classify typed Cloudflare/AWS failures into the stable codes table. Rate limits and service failures are retryable. Permission, bucket lock, identity, unsupported jurisdiction, missing credentials, persistent bucket-not-empty, and external writes are permanent for that job. On every terminal failure, call `MarkBucketDeletionFailed` if remote mutation may have occurred; otherwise restore the exact prior active state. Never restore a mutated bucket to active automatically.

**Step 8: Run state-machine tests**

```powershell
gofmt -w internal/modules/r2/bucket_deletion.go internal/modules/r2/bucket_deletion_jobs.go internal/modules/r2/bucket_deletion_jobs_test.go
go test ./internal/modules/r2 -run 'TestBucketDeletion' -count=1
```

Expected: PASS.

**Step 9: Commit the worker**

```powershell
git add internal/modules/r2/bucket_deletion.go internal/modules/r2/bucket_deletion_jobs.go internal/modules/r2/bucket_deletion_jobs_test.go
git commit -m "Add resumable R2 bucket deletion worker"
```

### Task 8: Expose a Safe Deletion HTTP API

**Files:**
- Add: `internal/platform/httpapi/r2_bucket_deletion.go`
- Add: `internal/platform/httpapi/r2_bucket_deletion_test.go`
- Modify: `internal/platform/httpapi/api.go`
- Modify: `internal/platform/httpapi/api_test.go`
- Modify: `internal/app/server.go`

**Step 1: Write failing endpoint tests**

Add:

```go
func TestCreateR2BucketDeletionRequiresAdminPassword(t *testing.T)
func TestCreateR2BucketDeletionRequiresExactName(t *testing.T)
func TestCreateR2BucketDeletionRejectsUnsupportedJurisdiction(t *testing.T)
func TestCreateR2BucketDeletionSnapshotsRemoteIdentity(t *testing.T)
func TestCreateR2BucketDeletionReturnsExistingActiveJob(t *testing.T)
func TestCreateR2BucketDeletionHandlesRemoteMissingManagedBucket(t *testing.T)
func TestDeleteLocalR2BucketExplainsNonEmptyFailureInChinese(t *testing.T)
func TestRemoteBucketViewsExposeDeletionLifecycle(t *testing.T)
```

Use the existing protected API fixture so session and CSRF behavior remain covered. Ensure neither password nor token appears in stored payload, response, or audit detail.

**Step 2: Run and verify failure**

```powershell
go test ./internal/platform/httpapi -run 'Test(CreateR2BucketDeletion|DeleteLocalR2BucketExplains|RemoteBucketViewsExpose)' -count=1
```

Expected: 404 or compilation failures for the new endpoint and fields.

**Step 3: Register and validate the endpoint**

Add:

```go
mux.Handle("POST /api/v1/r2/remote-buckets/{bucket_name}/deletions",
	api.protected(http.HandlerFunc(api.createR2BucketDeletion)))
```

The handler must:

1. decode with `DisallowUnknownFields`;
2. require account, mode, and admin password; require exact confirmation only for `empty_and_delete`;
3. authenticate the current admin password;
4. require `jurisdiction == "default"`;
5. load the account with secrets;
6. GET the exact remote bucket identity, or verify the managed local `remote_missing` case;
7. reject missing/unparseable creation date before enqueue;
8. derive resource key from manager account ID, jurisdiction, and bucket name;
9. call `EnqueueUnique`;
10. record an audit event without secrets and return 202.

For retry from `delete_failed`, accept `parent_job_id` only from the server-resolved current bucket state; do not trust an arbitrary client-supplied parent.

**Step 4: Expose lifecycle and job state in views**

Extend local bucket JSON and `remoteBucketView` with `jurisdiction`, `lifecycle_state`, `deletion_job_id`, `deletion_status`, `deletion_error_code`, and `deletion_error`. Use account + jurisdiction + name as the merge key, not name alone. A managed remote-missing bucket can schedule local finalization through the same endpoint.

**Step 5: Improve local deregistration failures**

Map `ErrBucketInUse` and foreign-key/object references to `409 bucket_not_empty` with:

`存储桶内仍有文件。请先删除桶内所有文件后再移出阵列；如需同时删除 Cloudflare 存储桶，请使用“一键清空并删除桶”。`

Do not leak SQLite constraint text.

**Step 6: Wire the real worker**

In `server.go`, construct:

```go
bucketDeletionJobs := r2.BucketDeletionJobs{
	Service: r2Service,
	Jobs:    jobStore,
	Remote:  accounts.RemoteClient{},
	Audit:   auditStore,
}
runner.Register(r2.BucketDeletionJobType, bucketDeletionJobs.Handle)
```

Use the same configured HTTP client/base URL conventions as other account clients. Keeping startup `ClearBucketMaintenanceLocks` is acceptable because persistent lifecycle state, not the runtime lock, is the safety boundary.

**Step 7: Run API/app tests**

```powershell
gofmt -w internal/platform/httpapi/r2_bucket_deletion.go internal/platform/httpapi/r2_bucket_deletion_test.go internal/platform/httpapi/api.go internal/platform/httpapi/api_test.go internal/app/server.go
go test ./internal/platform/httpapi ./internal/app -count=1
```

Expected: PASS.

**Step 8: Commit the HTTP boundary**

```powershell
git add internal/platform/httpapi internal/app/server.go
git commit -m "Expose safe remote R2 bucket deletion"
```

### Task 9: Preserve API Codes and Build the Dedicated Delete Dialog

**Files:**
- Modify: `web/src/api.ts`
- Modify: `web/src/types.ts`
- Add: `web/src/components/BucketDeleteDialog.tsx`
- Modify: `web/src/styles/components.css`
- Modify: `web/src/styles/responsive.css`

**Step 1: Extend API errors**

Change `APIError` to retain the backend code:

```ts
export class APIError extends Error {
  status: number;
  code: string;

  constructor(status: number, message: string, code = "") {
    super(message);
    this.status = status;
    this.code = code;
  }
}
```

In both fetch and XHR paths, parse `payload.error?.code` and pass it to the constructor. Preserve the existing unauthorized handler behavior.

**Step 2: Extend shared types**

Add lifecycle fields to `Bucket`, deletion fields and `jurisdiction` to `RemoteBucketView`, and `resource_key`, `error_code`, `parent_job_id`, plus payload counters/stage to `BackgroundJob`. Keep fields optional where older server responses remain possible during rolling updates.

**Step 3: Build `BucketDeleteDialog`**

The dialog must provide:

- a segmented/radio mode choice: “仅删除空桶” and “一键清空并删除桶”;
- an exact bucket-name confirmation input shown and required only for `empty_and_delete`;
- administrator password input;
- a warning that the operation is irreversible and external writes/recreation must stop;
- the selected bucket/account and object count when available;
- disabled confirmation until exact name/password requirements are met;
- inline stable Chinese errors based on `APIError.code`;
- no feature-description or keyboard-shortcut copy.

The destructive button label is “清空并删除” only for `empty_and_delete`; otherwise “删除空桶”. The local unlink command stays outside this dialog.

**Step 4: Add compact styles**

Use the existing dialog tokens and maximum 8px radius. Add stable widths with responsive max constraints, a non-card mode selector, progress rows, and mobile stacking. Do not nest cards or add decorative gradients.

**Step 5: Run frontend compile checks**

```powershell
npm --prefix web run lint
npm --prefix web run build
```

Expected: TypeScript and Vite complete successfully.

**Step 6: Commit the UI primitives**

```powershell
git add web/src/api.ts web/src/types.ts web/src/components/BucketDeleteDialog.tsx web/src/styles/components.css web/src/styles/responsive.css
git commit -m "Add R2 bucket deletion confirmation dialog"
```

### Task 10: Add Bucket Actions, Progress, Retry, and Chinese Failures

**Files:**
- Modify: `web/src/pages/StoragePage.tsx`

**Step 1: Separate local and remote actions**

Import Lucide `Unlink` and keep `Trash2` for remote destruction:

- `Unlink`: “移出阵列（不影响 Cloudflare 中的桶）”, opens the existing local `ConfirmDialog`.
- `Trash2`: “删除 Cloudflare 存储桶”, opens `BucketDeleteDialog` for managed and unmanaged rows.

Do not hide remote deletion behind local management state. Disable the remote delete button for non-default jurisdictions with the `unsupported_jurisdiction` tooltip/message.

**Step 2: Submit deletion jobs**

Post the selected mode, mode-dependent confirmation name, password, and account/jurisdiction. On `202`, close the dialog, retain the returned job ID, show “删除任务已创建”, and immediately refresh jobs and bucket views. Never retain the password in component state after close/submission.

**Step 3: Poll only while needed**

On page load, fetch `/api/v1/jobs?limit=200` and associate `r2.bucket.delete-remote` jobs by `resource_key`. While any visible deletion job is pending/running, poll every two seconds. Stop the timer when all visible jobs are terminal or the component unmounts. Refresh remote/local bucket views when a job transitions to terminal.

Display lifecycle/job status in the row:

- pending: “等待删除”;
- running: stage label plus deleted-object and aborted-upload counters;
- succeeded: refresh removes the remote row/local registration;
- failed: stable Chinese message and retry command.

Retry opens the same dialog, requests password/name confirmation again, and lets the server attach the failed parent job. Never retry silently.

**Step 4: Handle special cases**

- `remote_missing`: offer “完成本地清理” through the same job endpoint.
- `delete_failed`: keep the row visible and blocked from normal use.
- `bucket_not_empty`: show the explicit instruction and offer reopening with “一键清空并删除桶” selected.
- `local_finalize_failed`: explain that Cloudflare is already deleted and retry only completes local cleanup.
- unknown errors: show the backend message, not raw SQLite/SDK details.

**Step 5: Make activity and status views readable**

Extend `Status` so `deleting` uses the live treatment and `delete_failed` uses the bad treatment. In `ActivityPage`, use `BackgroundJob[]`; for `r2.bucket.delete-remote`, show the Chinese stage, deleted-object/aborted-upload counters, `error_code`, and the backend Chinese error. Do not duplicate Cloudflare error classification in the browser.

Document the endpoint in `docs/api.md`, including both modes, the mode-dependent exact-name rule, administrator-password requirement, `202` response, task resumption semantics, and the complete stable error table.

**Step 6: Run frontend checks**

```powershell
npm --prefix web run lint
npm --prefix web run build
```

Expected: PASS and `web/dist` is rebuilt.

**Step 7: Commit the page workflow**

```powershell
git add web/src/pages/StoragePage.tsx web/src/pages/ActivityPage.tsx web/src/components/UI.tsx web/src/styles/pages.css docs/api.md
git commit -m "Add remote R2 bucket deletion workflow"
```

### Task 11: Run Full Regression and Browser Acceptance

**Files:**
- Modify only files required by failures found in this task.
- Keep: `.openteams/specs/2026-08-30-r2-remote-bucket-deletion-design.html`
- Add: `.openteams/plans/2026-08-30-r2-remote-bucket-deletion.md`

**Step 1: Run focused backend suites**

```powershell
go test ./internal/platform/database ./internal/platform/jobs ./internal/platform/accounts ./internal/modules/r2 ./internal/platform/httpapi ./internal/protocol/s3 ./internal/protocol/webdav ./internal/app -count=1
```

Expected: PASS.

**Step 2: Run race-enabled full tests and build**

```powershell
go test -race -count=1 ./...
go build -trimpath ./cmd/cf-r2-manager
```

Expected: all packages pass; the binary builds.

**Step 3: Run frontend checks**

```powershell
npm --prefix web run lint
npm --prefix web run build
```

Expected: PASS.

**Step 4: Start a local server and verify the UI**

Start the application on an unused localhost port with a temporary database/config. In the browser, verify desktop and mobile widths:

1. local unlink uses `Unlink` and clearly says the Cloudflare bucket is unaffected;
2. remote deletion uses `Trash2` and opens the dedicated dialog;
3. exact-name and password validation prevent accidental submit;
4. both deletion modes are selectable;
5. pending/running/failed states do not shift or overlap table layout;
6. `bucket_not_empty` appears in Chinese and points to “一键清空并删除桶”;
7. non-default jurisdiction deletion is disabled;
8. a page reload restores active task progress.

Record screenshots only as disposable test artifacts; do not add them to the repository.

**Step 5: Test remote behavior only with disposable buckets**

After deployment, use temporary uniquely named buckets only:

- empty unmanaged bucket: `empty_only` deletes it;
- non-empty unmanaged bucket: `empty_only` fails in Chinese and leaves objects/bucket intact;
- non-empty bucket: `empty_and_delete` removes objects, multipart uploads, and bucket;
- managed bucket: protocol reads/writes return temporary-unavailable while deleting and local registration disappears only after remote success;
- forced permission failure: remote/local data remains and UI shows `permission_denied`;
- forced external write: task stops with `external_writes_detected`;
- refresh/restart during execution: the same resource job resumes without duplicate mutation.

Do not run destructive acceptance against `gamesync`, `webdav`, or any non-disposable bucket.

**Step 6: Check patch hygiene**

```powershell
git diff --check
git status --short
git diff --stat
```

Expected: no whitespace errors; only the approved design, plan, implementation, tests, and intended built frontend assets are changed.

**Step 7: Commit reviewed documentation and any final fixes**

```powershell
git add .openteams/specs/2026-08-30-r2-remote-bucket-deletion-design.html .openteams/plans/2026-08-30-r2-remote-bucket-deletion.md
git commit -m "Document safe R2 bucket deletion"
```

## Rollback Rule

Before rolling back to a binary that predates migration 007, query:

```sql
SELECT id, account_id, bucket_name, lifecycle_state, deletion_job_id
FROM r2_physical_buckets
WHERE lifecycle_state <> 'active';
```

Rollback is allowed only when this returns no rows. Older binaries ignore lifecycle fencing and therefore must never run while a bucket is `deleting` or `delete_failed`.
