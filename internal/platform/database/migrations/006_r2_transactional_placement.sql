ALTER TABLE r2_physical_buckets ADD COLUMN reserved_storage_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE r2_physical_buckets ADD COLUMN usage_checked_at INTEGER NOT NULL DEFAULT 0;

UPDATE r2_physical_buckets
SET storage_bytes = MAX(storage_bytes, COALESCE((
        SELECT SUM(size) FROM r2_objects
        WHERE physical_bucket_id = r2_physical_buckets.id AND state = 'committed'
    ), 0)),
    usage_checked_at = 0;

CREATE TABLE r2_write_intents (
    id TEXT PRIMARY KEY,
    object_key TEXT NOT NULL UNIQUE,
    target_bucket_id TEXT NOT NULL REFERENCES r2_physical_buckets(id),
    previous_object_id TEXT,
    reserved_bytes INTEGER NOT NULL DEFAULT 0,
    declared_size INTEGER NOT NULL DEFAULT -1,
    actual_size INTEGER NOT NULL DEFAULT 0,
    content_type TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    state TEXT NOT NULL,
    operation TEXT NOT NULL DEFAULT 'put',
    upstream_upload_id TEXT NOT NULL DEFAULT '',
    etag TEXT NOT NULL DEFAULT '',
    internal_multipart INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX r2_write_intents_bucket_idx ON r2_write_intents(target_bucket_id, state);

CREATE TABLE r2_bucket_maintenance_locks (
    physical_bucket_id TEXT PRIMARY KEY REFERENCES r2_physical_buckets(id) ON DELETE CASCADE,
    operation TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

ALTER TABLE r2_multipart_uploads ADD COLUMN write_intent_id TEXT REFERENCES r2_write_intents(id);

INSERT INTO r2_write_intents(
    id, object_key, target_bucket_id, previous_object_id, reserved_bytes, declared_size,
    actual_size, content_type, metadata_json, state, operation, upstream_upload_id, etag,
    internal_multipart, created_at, updated_at)
SELECT
    'legacy-multipart-' || upload.id,
    upload.object_key,
    upload.physical_bucket_id,
    (SELECT object_id FROM r2_objects WHERE object_key = upload.object_key AND state = 'committed'),
    COALESCE((SELECT SUM(size) FROM r2_multipart_parts WHERE upload_id = upload.id), 0),
    -1,
    0,
    COALESCE(json_extract(upload.metadata_json, '$.content_type'), ''),
    COALESCE(json_extract(upload.metadata_json, '$.metadata'), '{}'),
    CASE upload.status
        WHEN 'active' THEN 'uploading'
        WHEN 'completing' THEN 'completing'
        ELSE 'recovery'
    END,
    'legacy-multipart',
    upload.upstream_upload_id,
    '',
    0,
    upload.created_at,
    upload.updated_at
FROM r2_multipart_uploads AS upload
WHERE upload.id = (
    SELECT candidate.id FROM r2_multipart_uploads AS candidate
    WHERE candidate.object_key = upload.object_key
    ORDER BY candidate.created_at, candidate.id LIMIT 1
);

UPDATE r2_multipart_uploads
SET write_intent_id = 'legacy-multipart-' || id
WHERE EXISTS (SELECT 1 FROM r2_write_intents WHERE id = 'legacy-multipart-' || r2_multipart_uploads.id);

UPDATE r2_physical_buckets
SET reserved_storage_bytes = reserved_storage_bytes + COALESCE((
    SELECT SUM(reserved_bytes) FROM r2_write_intents
    WHERE target_bucket_id = r2_physical_buckets.id
), 0);

CREATE TABLE r2_multipart_part_reservations (
    upload_id TEXT NOT NULL REFERENCES r2_multipart_uploads(id) ON DELETE CASCADE,
    part_number INTEGER NOT NULL CHECK(part_number BETWEEN 1 AND 10000),
    previous_size INTEGER NOT NULL CHECK(previous_size >= 0),
    requested_size INTEGER NOT NULL CHECK(requested_size >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(upload_id, part_number)
);

CREATE TABLE r2_physical_cleanups (
    id TEXT PRIMARY KEY,
    object_key TEXT NOT NULL,
    physical_bucket_id TEXT NOT NULL REFERENCES r2_physical_buckets(id),
    physical_key TEXT NOT NULL,
    expected_etag TEXT NOT NULL DEFAULT '',
    size INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(physical_bucket_id, physical_key)
);
CREATE INDEX r2_physical_cleanups_status_idx ON r2_physical_cleanups(status, created_at);

CREATE TABLE r2_account_capacity (
    account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    unmanaged_storage_bytes INTEGER NOT NULL DEFAULT 0,
    usage_checked_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);

INSERT INTO r2_account_capacity(account_id, unmanaged_storage_bytes, usage_checked_at, updated_at)
SELECT DISTINCT account_id, 0, 0, unixepoch() FROM r2_physical_buckets;

CREATE TABLE r2_account_usage_monthly (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    usage_month TEXT NOT NULL,
    class_a_ops INTEGER NOT NULL DEFAULT 0,
    class_b_ops INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(account_id, usage_month)
);
