# Configuration

The server reads one YAML file. Production defaults live in
`packaging/config.yaml`; `config.example.yaml` uses workspace-relative paths for
local development.

## Paths and listeners

| Key | Production default | Purpose |
| --- | --- | --- |
| `data_dir` | `/opt/CloudFlareManager/data` | Persistent application state |
| `database_path` | `/opt/CloudFlareManager/data/manager.db` | SQLite WAL database |
| `master_key_file` | `/opt/CloudFlareManager/data/master.key` | Fallback key source when no systemd credential is injected |
| `listeners.http` | `0.0.0.0:14325` | Unified admin, S3, WebDAV, and OpenAI-compatible endpoint |
| `listeners.metrics` | `127.0.0.1:14329` | Prometheus metrics; loopback is enforced |

The unified listener keeps every protocol on one origin. The admin console,
S3 endpoint, and WebDAV endpoint use the listener root; the AI base URL uses
`/v1`. S3 requests are identified by SigV4, WebDAV by Basic authentication or
DAV methods, and AI by its supported `/v1` routes.

For upgrades, `listeners.admin` is accepted as a fallback when `listeners.http`
is absent. Legacy `listeners.s3`, `listeners.webdav`, and `listeners.ai` values
are ignored and logged; their ports are not opened.

The `master-key` file in `$CREDENTIALS_DIRECTORY` takes precedence over
`master_key_file`. It must contain exactly 32 raw bytes or their base64
encoding.

## R2 limits

`r2.logical_bucket` is the only bucket exposed to S3 clients. Physical buckets
are never exposed. The three soft limits are per physical bucket:

- `storage_soft_limit_bytes`: default 9 GB;
- `class_a_soft_limit`: default 900,000 operations;
- `class_b_soft_limit`: default 9,000,000 operations.

`r2.upload_chunk_bytes` (default 64 MiB, minimum 5 MiB) controls server-side
forced chunking: any single PUT larger than one chunk (or with unknown length)
is relayed to R2 through a multipart upload, so local disk usage peaks at one
chunk per concurrent transfer instead of the whole object. Lower it on hosts
with small disks; note each part consumes one Class A operation.

Placement stops using a bucket at 90% of a configured limit unless an
administrator has enabled a temporary overflow window. These are protective
local limits, not Cloudflare billing data.

## Workers AI

`ai.neuron_soft_limit` defaults to 9,000 estimated Neurons per account per UTC
day. The estimate is derived locally and must not be interpreted as the
Cloudflare invoice. `ai.max_retry_accounts` controls how many accounts may be
attempted before response streaming starts; a streaming request is never
replayed after bytes have been sent.
