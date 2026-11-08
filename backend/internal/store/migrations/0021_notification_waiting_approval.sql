-- docs/specs/17-approval.md 3.3 step 4: notify the owner that an approval is
-- waiting. Rebuild notification_requests to add 'waiting_approval' to the
-- status CHECK. Columns, values and the pending index are kept unchanged.
CREATE TABLE notification_requests_new (
    execution_id TEXT NOT NULL REFERENCES executions(id),
    status TEXT NOT NULL CHECK (status IN (
        'succeeded', 'failed', 'interrupted', 'timed_out', 'blocked', 'waiting_approval'
    )),
    created_at TEXT NOT NULL,
    attempted_at TEXT,
    PRIMARY KEY (execution_id, status)
);

INSERT INTO notification_requests_new (execution_id, status, created_at, attempted_at)
SELECT execution_id, status, created_at, attempted_at FROM notification_requests;

DROP TABLE notification_requests;
ALTER TABLE notification_requests_new RENAME TO notification_requests;

CREATE INDEX notification_requests_pending
    ON notification_requests(created_at, execution_id, status)
    WHERE attempted_at IS NULL;
