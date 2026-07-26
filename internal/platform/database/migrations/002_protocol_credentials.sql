ALTER TABLE protocol_credentials ADD COLUMN secret_id TEXT REFERENCES encrypted_secrets(id);

CREATE TABLE webdav_locks (
    token TEXT PRIMARY KEY,
    object_key TEXT NOT NULL,
    owner TEXT NOT NULL DEFAULT '',
    depth TEXT NOT NULL DEFAULT 'infinity',
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX webdav_locks_object_idx ON webdav_locks(object_key, expires_at);
