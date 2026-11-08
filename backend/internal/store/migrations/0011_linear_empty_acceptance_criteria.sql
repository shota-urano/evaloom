-- docs/specs/15-linear-intake.md 3.8 (decided 2026-11-08): acceptance_criteria
-- may be empty only for a Linear task held as missing_acceptance_criteria.
-- Manual tasks keep rejecting an empty value. SQLite cannot alter a CHECK,
-- so tasks is rebuilt; foreign keys are checked by the migration runner
-- before this table rebuild is committed.
CREATE TABLE tasks_acceptance_criteria (
    id TEXT PRIMARY KEY NOT NULL,
    project_id TEXT REFERENCES projects(id),
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    acceptance_criteria TEXT NOT NULL,
    short_name TEXT NOT NULL,
    task_type TEXT NOT NULL CHECK (task_type = 'development'),
    collaboration_mode TEXT NOT NULL CHECK (collaboration_mode = 'solo'),
    status TEXT NOT NULL CHECK (status IN (
        'received', 'planning', 'queued', 'running', 'verifying',
        'waiting_approval', 'completed', 'failed', 'cancelled', 'timed_out', 'blocked'
    )),
    idempotency_key TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'linear')),
    intake_block_cause TEXT CHECK (intake_block_cause IN (
        'routing_multiple_labels', 'routing_unknown_label', 'routing_team_mismatch',
        'routing_no_label', 'missing_acceptance_criteria', 'config_missing_assignment'
    )),
    CHECK (project_id IS NOT NULL OR (source = 'linear' AND intake_block_cause IS NOT NULL)),
    -- IS, not =: a NULL cause must fail the CHECK instead of passing it.
    CHECK (length(acceptance_criteria) > 0
        OR (source = 'linear' AND intake_block_cause IS 'missing_acceptance_criteria'))
);

INSERT INTO tasks_acceptance_criteria (
    id, project_id, title, body, acceptance_criteria, short_name, task_type,
    collaboration_mode, status, idempotency_key, created_at, updated_at,
    source, intake_block_cause
)
SELECT id, project_id, title, body, acceptance_criteria, short_name, task_type,
    collaboration_mode, status, idempotency_key, created_at, updated_at,
    source, intake_block_cause FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_acceptance_criteria RENAME TO tasks;
