-- Durable requests, not delivery receipts. The notification sender will record
-- notification_sent only after attempting delivery (spec 12).
CREATE TABLE notification_requests (
    execution_id TEXT NOT NULL REFERENCES executions(id),
    status TEXT NOT NULL CHECK (status IN ('succeeded', 'failed', 'interrupted', 'timed_out', 'blocked')),
    created_at TEXT NOT NULL,
    PRIMARY KEY (execution_id, status)
);
