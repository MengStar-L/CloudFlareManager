CREATE TABLE r2_multipart_parts (
    upload_id TEXT NOT NULL REFERENCES r2_multipart_uploads(id) ON DELETE CASCADE,
    part_number INTEGER NOT NULL CHECK(part_number BETWEEN 1 AND 10000),
    etag TEXT NOT NULL,
    size INTEGER NOT NULL CHECK(size >= 0),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(upload_id, part_number)
);

CREATE INDEX r2_multipart_status_idx ON r2_multipart_uploads(status, created_at);
