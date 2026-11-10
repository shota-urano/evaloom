-- docs/specs/19-routing-and-stages.md 3.11 (Q170, decided 2026-11-10):
-- the development work (worktree, Gate 2/3, Git Publisher, test command,
-- setup, copy allowlist, Project Context) moved to the special harness
-- software-engineer. docs/specs/02-data-model.md keeps no development
-- columns or tables. Remove them here. Nothing is in operational use, and
-- the previous schema can be read from the git history.
-- The CHECK values of the remaining columns are kept unchanged.
-- The migration runner disables foreign keys for the table rebuilds and runs
-- foreign_key_check before commit.

DROP TABLE execution_blocks;
DROP TABLE gate_results;
DROP TABLE test_runs;

CREATE TABLE executions_new (
    id TEXT PRIMARY KEY NOT NULL,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    seq INTEGER NOT NULL CHECK (seq >= 1),
    mode TEXT NOT NULL CHECK (mode IN ('continue', 'fresh')),
    status TEXT NOT NULL CHECK (status IN (
        'pending', 'claimed', 'preparing', 'running', 'verifying', 'publishing',
        'waiting_approval', 'merging',
        'succeeded', 'blocked', 'failed', 'interrupted', 'cancelled', 'timed_out'
    )),
    runner_id TEXT REFERENCES runners(id),
    role TEXT,
    harness_id TEXT REFERENCES harnesses(id),
    profile_id TEXT REFERENCES harness_profiles(id),
    profile_version TEXT,
    profile_commit_sha TEXT,
    harness_session_id TEXT,
    error_class TEXT CHECK (error_class IN (
        'config_error', 'execution_failed', 'test_failed', 'timeout',
        'cancelled', 'interrupted', 'publish_failed', 'sandbox_error'
    )),
    error_message TEXT,
    cancel_requested_at TEXT,
    claimed_at TEXT,
    started_at TEXT,
    finished_at TEXT,
    last_heartbeat_at TEXT,
    created_at TEXT NOT NULL,
    UNIQUE (task_id, seq)
);

INSERT INTO executions_new (
    id, task_id, seq, mode, status, runner_id, role, harness_id, profile_id,
    profile_version, profile_commit_sha, harness_session_id, error_class,
    error_message, cancel_requested_at, claimed_at, started_at, finished_at,
    last_heartbeat_at, created_at
) SELECT
    id, task_id, seq, mode, status, runner_id, role, harness_id, profile_id,
    profile_version, profile_commit_sha, harness_session_id, error_class,
    error_message, cancel_requested_at, claimed_at, started_at, finished_at,
    last_heartbeat_at, created_at
FROM executions;

DROP TABLE executions;
ALTER TABLE executions_new RENAME TO executions;

DROP TABLE worktrees;

CREATE TABLE projects_new (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    runner_id TEXT NOT NULL REFERENCES runners(id),
    default_role TEXT NOT NULL,
    review_role TEXT,
    allowed_operations TEXT NOT NULL,
    timeouts TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    linear_team_id TEXT,
    linear_routing_label TEXT
);

INSERT INTO projects_new (
    id, name, runner_id, default_role, review_role, allowed_operations,
    timeouts, created_at, updated_at, linear_team_id, linear_routing_label
) SELECT
    id, name, runner_id, default_role, review_role, allowed_operations,
    timeouts, created_at, updated_at, linear_team_id, linear_routing_label
FROM projects;

DROP TABLE projects;
ALTER TABLE projects_new RENAME TO projects;

CREATE UNIQUE INDEX projects_linear_team_routing_label
    ON projects(linear_team_id, linear_routing_label);
