CREATE TABLE r2_scan_findings (
    id TEXT PRIMARY KEY,
    physical_bucket_id TEXT NOT NULL REFERENCES r2_physical_buckets(id) ON DELETE CASCADE,
    physical_key TEXT NOT NULL,
    kind TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    found_at INTEGER NOT NULL,
    UNIQUE(physical_bucket_id, physical_key, kind)
);

CREATE INDEX r2_scan_findings_bucket_idx ON r2_scan_findings(physical_bucket_id, kind, found_at DESC);
