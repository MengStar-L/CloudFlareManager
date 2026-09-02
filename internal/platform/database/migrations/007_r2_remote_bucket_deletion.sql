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

CREATE TRIGGER r2_physical_buckets_block_active_remote_deletion
BEFORE INSERT ON r2_physical_buckets
WHEN EXISTS (
    SELECT 1 FROM jobs
    WHERE type = 'r2.bucket.delete-remote'
      AND resource_key = NEW.account_id || '/default/' || NEW.bucket_name
      AND status IN ('pending', 'running')
)
BEGIN
    SELECT RAISE(ABORT, 'r2_remote_bucket_deletion_active');
END;
