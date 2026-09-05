# HTTP API

The management API is rooted at `/api/v1`. Login returns an HttpOnly session
cookie and a CSRF token. Every non-read management request after login must send
that value in `X-CSRF-Token`.

## Platform

- `POST /api/v1/session`, `GET /api/v1/session`, `DELETE /api/v1/session`
- `GET|POST /api/v1/accounts`, `GET|DELETE /api/v1/accounts/{id}`
- `PATCH /api/v1/accounts/{id}/credentials`
- `POST /api/v1/accounts/{id}/verify`
- `GET /api/v1/jobs`, `GET /api/v1/events`, `GET /api/v1/audit`
- `GET /api/v1/system/endpoints`
- `GET|POST /api/v1/credentials`
- `POST /api/v1/credentials/{id}/rotate`
- `DELETE /api/v1/credentials/{id}`

S3, WebDAV, and AI credentials are separate identities. Their returned secret
is shown only when created or rotated.

Cloudflare account credentials are updated in place with
`PATCH /api/v1/accounts/{id}/credentials`. Omitted fields keep their current
values; `r2_access_key_id` and `r2_secret_access_key` must be supplied together,
and `clear_r2_credentials` explicitly removes both. The Cloudflare Account ID
is immutable so existing bucket and object mappings cannot be redirected to a
different remote account. Removing R2 credentials preserves those mappings;
until credentials are configured again, the management file API returns JSON
`503 r2_credentials_required`, S3 returns XML `503 ServiceUnavailable`, and
WebDAV returns HTTP `503 Service Unavailable`.

Account verification reports the failed check, request method/path, HTTP status,
Cloudflare error code/message when provided, and troubleshooting guidance in
`capabilities[].detail`. If both user-token and account-token verification fail,
both endpoint results are retained. `health_error` contains the token failure,
a list of unavailable capabilities, or a connection/response error when the
result cannot be confirmed. API tokens are redacted from these diagnostics.
The R2 capability probe checks bucket-list access using the API token; it does
not validate the separate S3 access key pair. Existing checks must be rerun to
collect the additional diagnostic information.

Creating an account queues one capability-detection job automatically. Account
responses include `verification: {job_id, status, attempts}` while a check is
pending or running, including retry waits. This transient state is read from
persisted jobs and survives page reloads; it is omitted after all checks finish.
The account page polls while verification is active, then displays the final
health and capability results without a manual refresh.

Deleting a Cloudflare account is blocked while it still owns registered R2
buckets or has an active remote-bucket deletion job. `DELETE
/api/v1/accounts/{id}` returns `409 account_in_use` with
`error.details.blockers`, including each blocker kind, total count, and a
bounded preview of resource names. Remove or finish those resources from the
R2 Storage or Activity page, then retry. Account-owned AI request history does
not block deletion and is removed with the account.

## R2

- `GET|POST /api/v1/r2/buckets`
- `DELETE /api/v1/r2/buckets/{id}`
- `GET|POST /api/v1/r2/remote-buckets`
- `POST /api/v1/r2/remote-buckets/{bucket_name}/deletions`
- `POST /api/v1/r2/buckets/{id}/adopt`
- `POST /api/v1/r2/buckets/{id}/orphans/scan`
- `GET /api/v1/r2/objects`
- `GET /api/v1/r2/findings`
- `POST /api/v1/r2/index/rebuild`
- `POST /api/v1/r2/recovery`
- `POST /api/v1/r2/rebalance`

Adoption and rebuild operations skip conflicting logical keys. Orphan scans are
report-only and never delete R2 data. Rebalance requires distinct
`source_bucket_id` and `target_bucket_id`; an optional `prefix` narrows the move.
The bucket list response retains per-bucket fields and adds
`reserved_storage_bytes` and `usage_checked_at`. Its `account_usage` collection
reports managed, unmanaged, and reserved bytes, the account storage limit,
Class A/B usage and limits, and the current UTC usage month.

`DELETE /api/v1/r2/buckets/{id}` removes only the local array registration. It
never deletes the Cloudflare bucket or its objects. Remote deletion is an
asynchronous, resource-unique job created with:

```json
{
  "account_id": "local-account-id",
  "jurisdiction": "default",
  "mode": "empty_only",
  "confirmation_name": "",
  "admin_password": "current administrator password"
}
```

`empty_only` deletes the remote bucket only when it is already empty and never
removes objects. `empty_and_delete` first removes every object and incomplete
multipart upload, then deletes the bucket; it also requires
`confirmation_name` to exactly match the path bucket name. This release allows
destructive deletion only in the `default` R2 jurisdiction. The response is
`202 Accepted` with a background job. Deletion progress and stable error codes
are available from `GET /api/v1/jobs?type=r2.bucket.delete-remote`; active jobs
temporarily fence the managed bucket from new S3, WebDAV, and management writes.

## D1

- `GET|POST /api/v1/d1/databases`
- `DELETE /api/v1/d1/databases/{id}`
- `POST /api/v1/d1/databases/{id}/query`
- `GET /api/v1/d1/databases/{id}/schema`
- `GET /api/v1/d1/databases/{id}/tables/{table}/rows`
- `GET /api/v1/d1/databases/{id}/history`
- `GET /api/v1/d1/databases/{id}/insights`
- `POST /api/v1/d1/databases/{id}/backup`
- `GET /api/v1/d1/databases/{id}/backups`
- `POST /api/v1/d1/databases/{id}/time-travel/restore`

SQL is read-only by default. Any statement not conservatively classified as a
read requires the administrator password in the request. A Time Travel restore
does not start unless the pre-restore export has been committed to the unified
R2 pool.

## Workers AI

The unified HTTP listener exposes `/v1/models`, `/v1/chat/completions`, `/v1/responses`,
`/v1/embeddings`, and `/v1/run/{model}`. Management endpoints expose usage,
metadata-only request logs, AI Gateway CRUD/logs, and the admin playground under
`/api/v1/ai`.

`GET /v1/models` authenticates with the same AI Bearer token and returns the
standard OpenAI `list` response. `GET /api/v1/system/endpoints` is an
administrator endpoint that reports the effective public URLs and S3 logical
bucket for configuring clients.

Prompt and response bodies are not persisted by default.
