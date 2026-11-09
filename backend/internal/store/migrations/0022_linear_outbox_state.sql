-- Spec 16 3.6.3, 3.7: comments and Workflow state changes share the outbox.
-- Rebuild to allow NULL bodies, new kinds and skipped rows. The migration
-- runner checks foreign keys before committing the copied rows.
CREATE TABLE linear_outbox_new (
    id TEXT PRIMARY KEY NOT NULL,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    linear_issue_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN (
        'intake_blocked', 'missing_acceptance_criteria', 'blocked', 'waiting_approval',
        'succeeded', 'failed', 'merged', 'cancelled', 'started', 'completed'
    )),
    action TEXT NOT NULL DEFAULT 'comment' CHECK (action IN ('comment', 'set_state')),
    state_name TEXT,
    dedupe_key TEXT NOT NULL UNIQUE,
    body TEXT,
    status TEXT NOT NULL CHECK (status IN ('pending', 'sent', 'failed', 'skipped')),
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT,
    linear_comment_id TEXT,
    created_at TEXT NOT NULL,
    sent_at TEXT,
    outcome_unknown INTEGER NOT NULL DEFAULT 0 CHECK (outcome_unknown IN (0, 1)),
    execution_id TEXT REFERENCES executions(id),
    failure_reason TEXT CHECK (failure_reason IS NULL OR failure_reason IN (
        'auth', 'issue_removed', 'retries_exhausted', 'other'
    )),
    failed_at TEXT,
    CHECK ((action = 'comment' AND state_name IS NULL AND body IS NOT NULL)
        OR (action = 'set_state' AND state_name IS NOT NULL AND body IS NULL
            AND outcome_unknown = 0 AND linear_comment_id IS NULL))
);

INSERT INTO linear_outbox_new (
    id, task_id, linear_issue_id, kind, dedupe_key, body, status, attempts,
    next_attempt_at, linear_comment_id, created_at, sent_at, outcome_unknown,
    execution_id, failure_reason, failed_at
) SELECT
    id, task_id, linear_issue_id, kind, dedupe_key, body, status, attempts,
    next_attempt_at, linear_comment_id, created_at, sent_at, outcome_unknown,
    execution_id, failure_reason, failed_at
FROM linear_outbox;

DROP TABLE linear_outbox;
ALTER TABLE linear_outbox_new RENAME TO linear_outbox;
