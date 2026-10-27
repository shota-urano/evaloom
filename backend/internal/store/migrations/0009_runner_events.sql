-- Stable source IDs make retries safe when the commit succeeded but the HTTP
-- response was lost. Keep receipts for as long as their Execution is retained.
CREATE TABLE runner_event_receipts (
    source_id TEXT PRIMARY KEY,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    payload_hash TEXT NOT NULL
);
