# Configuration

The server reads one YAML file. Production defaults live in
`packaging/config.yaml`; `config.example.yaml` uses workspace-relative paths for
local development.

## Paths and listeners

| Key | Production default | Purpose |
| --- | --- | --- |
| `data_dir` | `/var/lib/cf-r2-manager` | Persistent application state |
| `database_path` | `/var/lib/cf-r2-manager/manager.db` | SQLite WAL database |
| `master_key_file` | `/var/lib/cf-r2-manager/master.key` | Fallback key source when no systemd credential is injected |
| `listeners.admin` | `127.0.0.1:8080` | Admin API and embedded console |
| `listeners.s3` | `127.0.0.1:9000` | S3-compatible endpoint |
| `listeners.webdav` | `127.0.0.1:9001` | WebDAV endpoint |
| `listeners.ai` | `127.0.0.1:9002` | Workers AI/OpenAI-compatible endpoint |
| `listeners.metrics` | `127.0.0.1:9090` | Prometheus metrics; loopback is enforced |

The `master-key` file in `$CREDENTIALS_DIRECTORY` takes precedence over
`master_key_file`. It must contain exactly 32 raw bytes or their base64
encoding.

## R2 limits

`r2.logical_bucket` is the only bucket exposed to S3 clients. Physical buckets
are never exposed. The three soft limits are per physical bucket:

- `storage_soft_limit_bytes`: default 9 GB;
- `class_a_soft_limit`: default 900,000 operations;
- `class_b_soft_limit`: default 9,000,000 operations.

Placement stops using a bucket at 90% of a configured limit unless an
administrator has enabled a temporary overflow window. These are protective
local limits, not Cloudflare billing data.

## Workers AI

`ai.neuron_soft_limit` defaults to 9,000 estimated Neurons per account per UTC
day. The estimate is derived locally and must not be interpreted as the
Cloudflare invoice. `ai.max_retry_accounts` controls how many accounts may be
attempted before response streaming starts; a streaming request is never
replayed after bytes have been sent.
