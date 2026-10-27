-- Rebuild to extend the event-type CHECK without changing existing receipts
-- or SSE cursor IDs. This migration runs in a single transaction.
CREATE TABLE execution_events_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    type TEXT NOT NULL CHECK (type IN (
        'status_changed', 'log', 'worktree_ready', 'context_pinned',
        'harness_started', 'harness_finished', 'gate_result', 'blocked',
        'pushed', 'pr_created', 'notification_sent', 'notification_failed'
    )),
    payload TEXT NOT NULL,
    created_at TEXT NOT NULL
);

INSERT INTO execution_events_new (id, execution_id, type, payload, created_at)
    SELECT id, execution_id, type, payload, created_at FROM execution_events;

-- Preserve even IDs whose events have already been deleted.
UPDATE sqlite_sequence SET seq = max(seq, coalesce(
    (SELECT seq FROM sqlite_sequence WHERE name = 'execution_events'), 0))
    WHERE name = 'execution_events_new';

DROP TABLE execution_events;
ALTER TABLE execution_events_new RENAME TO execution_events;
CREATE INDEX execution_events_execution_id_id ON execution_events(execution_id, id);
