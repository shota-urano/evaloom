-- docs/specs/15-linear-intake.md 3.8. Foreign keys are checked by the
-- migration runner before committing this table rebuild.
ALTER TABLE projects ADD COLUMN linear_team_id TEXT;
ALTER TABLE projects ADD COLUMN linear_routing_label TEXT;
CREATE UNIQUE INDEX projects_linear_team_routing_label
    ON projects(linear_team_id, linear_routing_label);

CREATE TABLE tasks_linear_intake (
    id TEXT PRIMARY KEY NOT NULL,
    project_id TEXT REFERENCES projects(id),
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    acceptance_criteria TEXT NOT NULL CHECK (length(acceptance_criteria) > 0),
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
    CHECK (project_id IS NOT NULL OR (source = 'linear' AND intake_block_cause IS NOT NULL))
);

INSERT INTO tasks_linear_intake (
    id, project_id, title, body, acceptance_criteria, short_name, task_type,
    collaboration_mode, status, idempotency_key, created_at, updated_at
)
SELECT id, project_id, title, body, acceptance_criteria, short_name, task_type,
    collaboration_mode, status, idempotency_key, created_at, updated_at FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_linear_intake RENAME TO tasks;

CREATE TABLE linear_issue_states (
    linear_issue_id TEXT PRIMARY KEY NOT NULL,
    issue_identifier TEXT NOT NULL,
    team_id TEXT NOT NULL,
    ai_ready INTEGER NOT NULL CHECK (ai_ready IN (0, 1)),
    attach_seq INTEGER NOT NULL,
    issue_updated_at TEXT NOT NULL,
    last_decision TEXT,
    last_decision_at TEXT,
    first_seen_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL
);

CREATE TABLE linear_poll_teams (
    team_id TEXT PRIMARY KEY NOT NULL,
    baseline_done_at TEXT,
    last_success_at TEXT,
    last_error TEXT CHECK (last_error IN ('auth', 'rate_limited', 'network', 'other')),
    consecutive_failures INTEGER NOT NULL
);

CREATE TABLE linear_issue_links (
    id TEXT PRIMARY KEY NOT NULL,
    linear_issue_id TEXT NOT NULL,
    issue_identifier TEXT NOT NULL,
    issue_url TEXT NOT NULL,
    team_id TEXT NOT NULL,
    task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id),
    attach_seq INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (linear_issue_id, attach_seq)
);
