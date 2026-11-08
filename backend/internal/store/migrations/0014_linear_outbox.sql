-- docs/specs/16-linear-feedback.md 3.7: one row per Linear comment to send.
-- Rows are written in the same transaction as the state change that causes
-- them (3.4 step 1). UNIQUE(dedupe_key) keeps one comment per trigger.
CREATE TABLE linear_outbox (
    id TEXT PRIMARY KEY NOT NULL,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    linear_issue_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN (
        'intake_blocked', 'missing_acceptance_criteria', 'blocked', 'waiting_approval',
        'succeeded', 'failed', 'merged', 'cancelled'
    )),
    dedupe_key TEXT NOT NULL UNIQUE,
    body TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'sent', 'failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT,
    linear_comment_id TEXT,
    created_at TEXT NOT NULL,
    sent_at TEXT
);
