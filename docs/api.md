# HTTP API

The management API is rooted at `/api/v1`. Login returns an HttpOnly session
cookie and a CSRF token. Every non-read management request after login must send
that value in `X-CSRF-Token`.

## Platform

- `POST /api/v1/session`, `GET /api/v1/session`, `DELETE /api/v1/session`
- `GET|POST /api/v1/accounts`, `GET|DELETE /api/v1/accounts/{id}`
- `GET /api/v1/jobs`, `GET /api/v1/events`, `GET /api/v1/audit`
- `GET|POST /api/v1/credentials`
- `POST /api/v1/credentials/{id}/rotate`
- `DELETE /api/v1/credentials/{id}`

S3, WebDAV, and AI credentials are separate identities. Their returned secret
is shown only when created or rotated.

## R2

- `GET|POST /api/v1/r2/buckets`
- `DELETE /api/v1/r2/buckets/{id}`
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

The AI listener exposes `/v1/models`, `/v1/chat/completions`, `/v1/responses`,
`/v1/embeddings`, and `/v1/run/{model}`. Management endpoints expose usage,
metadata-only request logs, AI Gateway CRUD/logs, and the admin playground under
`/api/v1/ai`.

Prompt and response bodies are not persisted by default.
