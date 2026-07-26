CREATE TABLE system_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE admin_users (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    password_hash TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE sessions (
    token_hash BLOB PRIMARY KEY,
    csrf_token TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE encrypted_secrets (
    id TEXT PRIMARY KEY,
    scope TEXT NOT NULL,
    kind TEXT NOT NULL,
    ciphertext BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE accounts (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    cloudflare_account_id TEXT NOT NULL UNIQUE,
    api_token_secret_id TEXT NOT NULL REFERENCES encrypted_secrets(id),
    r2_access_key_id_secret_id TEXT REFERENCES encrypted_secrets(id),
    r2_secret_access_key_secret_id TEXT REFERENCES encrypted_secrets(id),
    enabled INTEGER NOT NULL DEFAULT 1,
    health_status TEXT NOT NULL DEFAULT 'unknown',
    health_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE account_capabilities (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    capability TEXT NOT NULL,
    available INTEGER NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    checked_at INTEGER NOT NULL,
    PRIMARY KEY (account_id, capability)
);

CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    status TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    progress REAL NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 4,
    error TEXT NOT NULL DEFAULT '',
    lease_until INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX jobs_status_created_idx ON jobs(status, created_at);

CREATE TABLE audit_events (
    id TEXT PRIMARY KEY,
    actor TEXT NOT NULL,
    protocol TEXT NOT NULL,
    action TEXT NOT NULL,
    resource TEXT NOT NULL,
    result TEXT NOT NULL,
    request_id TEXT NOT NULL,
    detail_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL
);
CREATE INDEX audit_events_created_idx ON audit_events(created_at DESC);

CREATE TABLE protocol_credentials (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    public_id TEXT NOT NULL UNIQUE,
    secret_hash BLOB NOT NULL,
    scopes_json TEXT NOT NULL,
    disabled INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE usage_samples (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    product TEXT NOT NULL,
    resource_id TEXT NOT NULL DEFAULT '',
    metric TEXT NOT NULL,
    value REAL NOT NULL,
    sampled_at INTEGER NOT NULL
);
CREATE INDEX usage_samples_lookup_idx ON usage_samples(account_id, product, metric, sampled_at DESC);

CREATE TABLE r2_physical_buckets (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    bucket_name TEXT NOT NULL,
    writable INTEGER NOT NULL DEFAULT 1,
    adopted INTEGER NOT NULL DEFAULT 0,
    storage_bytes INTEGER NOT NULL DEFAULT 0,
    class_a_ops INTEGER NOT NULL DEFAULT 0,
    class_b_ops INTEGER NOT NULL DEFAULT 0,
    latency_ms REAL NOT NULL DEFAULT 0,
    health_status TEXT NOT NULL DEFAULT 'unknown',
    allow_overflow_until INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(account_id, bucket_name)
);

CREATE TABLE r2_placement_rules (
    id TEXT PRIMARY KEY,
    priority INTEGER NOT NULL,
    prefix TEXT NOT NULL DEFAULT '',
    extension TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL DEFAULT '',
    min_size INTEGER NOT NULL DEFAULT 0,
    max_size INTEGER NOT NULL DEFAULT 0,
    target_bucket_id TEXT NOT NULL REFERENCES r2_physical_buckets(id) ON DELETE CASCADE,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE r2_objects (
    object_key TEXT PRIMARY KEY,
    object_id TEXT NOT NULL UNIQUE,
    physical_bucket_id TEXT NOT NULL REFERENCES r2_physical_buckets(id),
    physical_key TEXT NOT NULL,
    state TEXT NOT NULL,
    size INTEGER NOT NULL DEFAULT 0,
    etag TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    last_modified INTEGER NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX r2_objects_list_idx ON r2_objects(state, object_key);

CREATE TABLE r2_multipart_uploads (
    id TEXT PRIMARY KEY,
    object_key TEXT NOT NULL,
    object_id TEXT NOT NULL,
    physical_bucket_id TEXT NOT NULL REFERENCES r2_physical_buckets(id),
    upstream_upload_id TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    status TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE d1_query_history (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    database_id TEXT NOT NULL,
    sql_text TEXT NOT NULL,
    sql_class TEXT NOT NULL,
    success INTEGER NOT NULL,
    rows_read INTEGER NOT NULL DEFAULT 0,
    rows_written INTEGER NOT NULL DEFAULT 0,
    duration_ms REAL NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE d1_backups (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    database_id TEXT NOT NULL,
    r2_object_key TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE ai_usage_daily (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    usage_date TEXT NOT NULL,
    estimated_neurons REAL NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    requests INTEGER NOT NULL DEFAULT 0,
    errors INTEGER NOT NULL DEFAULT 0,
    latency_ms_total REAL NOT NULL DEFAULT 0,
    PRIMARY KEY(account_id, usage_date)
);

CREATE TABLE ai_request_logs (
    id TEXT PRIMARY KEY,
    protocol_credential_id TEXT,
    account_id TEXT NOT NULL REFERENCES accounts(id),
    model TEXT NOT NULL,
    status_code INTEGER NOT NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    estimated_neurons REAL NOT NULL DEFAULT 0,
    duration_ms REAL NOT NULL DEFAULT 0,
    error_class TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX ai_request_logs_created_idx ON ai_request_logs(created_at DESC);

