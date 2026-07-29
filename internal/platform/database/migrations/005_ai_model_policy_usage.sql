CREATE TABLE ai_paid_models (
    model_id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    detected_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

ALTER TABLE ai_request_logs
    ADD COLUMN neuron_estimation_source TEXT NOT NULL DEFAULT 'legacy';

CREATE INDEX ai_request_logs_usage_detail_idx
    ON ai_request_logs(account_id, created_at, protocol_credential_id);
