-- Spec 02, sections 3.3 and 5. Existing rows must satisfy the new contract;
-- ambiguous obsolete states/events/context are rejected, never discarded.
CREATE TABLE tasks_new (
    id TEXT PRIMARY KEY NOT NULL,
    source TEXT NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'linear')),
    project_id TEXT REFERENCES projects(id),
    harness_id TEXT REFERENCES harnesses(id),
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    acceptance_criteria TEXT NOT NULL,
    collaboration_mode TEXT NOT NULL CHECK (collaboration_mode = 'solo'),
    status TEXT NOT NULL CHECK (status IN (
        'received', 'routing', 'queued', 'running', 'waiting_approval', 'blocked',
        'completed', 'failed', 'cancelled', 'timed_out'
    )),
    intake_block_cause TEXT CHECK (intake_block_cause IN (
        'missing_acceptance_criteria', 'config_missing_profile'
    )),
    idempotency_key TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (project_id IS NULL OR harness_id IS NULL)
);

INSERT INTO tasks_new (
    id, source, project_id, title, body, acceptance_criteria, collaboration_mode,
    status, intake_block_cause, idempotency_key, created_at, updated_at
) SELECT id, source, project_id, title, body, acceptance_criteria, collaboration_mode,
    CASE status WHEN 'verifying' THEN 'running' ELSE status END,
    intake_block_cause, idempotency_key, created_at, updated_at
FROM tasks;

CREATE TABLE executions_new (
    id TEXT PRIMARY KEY NOT NULL,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    seq INTEGER NOT NULL CHECK (seq >= 1),
    mode TEXT NOT NULL CHECK (mode IN ('continue', 'fresh')),
    status TEXT NOT NULL CHECK (status IN (
        'pending', 'claimed', 'preparing', 'working', 'checking', 'publishing',
        'waiting_approval', 'blocked', 'succeeded', 'failed', 'interrupted',
        'cancelled', 'timed_out'
    )),
    resume_stage TEXT CHECK (resume_stage IN ('work', 'publish')),
    runner_id TEXT REFERENCES runners(id),
    route_kind TEXT CHECK (route_kind IN ('project', 'harness')),
    project_id TEXT,
    harness_id TEXT,
    profile_id TEXT,
    profile_version TEXT,
    profile_commit_sha TEXT,
    contract_version TEXT,
    params TEXT,
    workspace_id TEXT REFERENCES task_workspaces(id),
    work_session_id TEXT,
    content_id TEXT,
    outputs TEXT,
    error_class TEXT CHECK (error_class IN (
        'config_error', 'prepare_failed', 'work_failed', 'check_failed',
        'publish_failed', 'publish_unknown', 'approval_rejected', 'contract_error',
        'sandbox_error', 'timeout', 'cancelled', 'interrupted'
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

-- A legacy execution has only Project routing. Preserve pins that were saved;
-- do not fill missing claim-time params/contract from mutable registry values.
INSERT INTO executions_new (
    id, task_id, seq, mode, status, runner_id, route_kind, project_id,
    harness_id, profile_id, profile_version, profile_commit_sha, work_session_id,
    error_class, error_message, cancel_requested_at, claimed_at, started_at,
    finished_at, last_heartbeat_at, created_at
) SELECT e.id, e.task_id, e.seq, e.mode,
    CASE e.status WHEN 'running' THEN 'working' WHEN 'verifying' THEN 'checking' ELSE e.status END,
    e.runner_id, CASE WHEN e.profile_id IS NOT NULL THEN 'project' END,
    CASE WHEN e.profile_id IS NOT NULL THEN t.project_id END,
    e.harness_id, e.profile_id, e.profile_version, e.profile_commit_sha, e.harness_session_id,
    CASE e.error_class WHEN 'execution_failed' THEN 'work_failed' WHEN 'test_failed' THEN 'check_failed' ELSE e.error_class END,
    e.error_message, e.cancel_requested_at, e.claimed_at, e.started_at,
    e.finished_at, e.last_heartbeat_at, e.created_at
FROM executions e JOIN tasks t ON t.id = e.task_id;

CREATE TABLE task_workspaces (
    id TEXT PRIMARY KEY NOT NULL,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    runner_id TEXT NOT NULL REFERENCES runners(id),
    path TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('active', 'retained', 'cleaned')),
    created_by_execution_id TEXT NOT NULL REFERENCES executions(id),
    created_at TEXT NOT NULL,
    cleaned_at TEXT
);

CREATE TABLE stage_runs (
    id TEXT PRIMARY KEY NOT NULL,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    stage TEXT NOT NULL CHECK (stage IN ('prepare', 'work', 'check', 'publish')),
    attempt INTEGER NOT NULL CHECK (attempt >= 1),
    status TEXT NOT NULL CHECK (status IN (
        'running', 'succeeded', 'failed', 'blocked', 'needs_approval', 'unknown',
        'question', 'cancelled', 'timed_out'
    )),
    exit_code INTEGER,
    error_class TEXT CHECK (error_class IN (
        'config_error', 'prepare_failed', 'work_failed', 'check_failed',
        'publish_failed', 'publish_unknown', 'approval_rejected', 'contract_error',
        'sandbox_error', 'timeout', 'cancelled', 'interrupted'
    )),
    request_artifact_id TEXT REFERENCES artifacts(id),
    result_artifact_id TEXT REFERENCES artifacts(id),
    started_at TEXT NOT NULL,
    finished_at TEXT,
    UNIQUE (execution_id, stage, attempt)
);

CREATE TABLE check_results (
    id TEXT PRIMARY KEY NOT NULL,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    stage_run_id TEXT NOT NULL REFERENCES stage_runs(id),
    result TEXT NOT NULL CHECK (result IN ('pass', 'fail', 'blocked')),
    content_id TEXT,
    summary TEXT,
    findings TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE execution_events_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    type TEXT NOT NULL CHECK (type IN (
        'status_changed', 'log', 'stage_started', 'stage_finished', 'context_pinned',
        'check_result', 'approval_requested', 'approval_waived', 'approval_consumed',
        'decision_opened', 'decision_answered', 'published', 'notification_sent', 'notification_failed'
    )),
    payload TEXT NOT NULL,
    created_at TEXT NOT NULL
);

INSERT INTO execution_events_new SELECT * FROM execution_events;
-- Keep the high-water mark even when the highest SSE event was deleted.
UPDATE sqlite_sequence SET seq = max(seq, coalesce(
    (SELECT seq FROM sqlite_sequence WHERE name = 'execution_events'), 0
)) WHERE name = 'execution_events_new';

CREATE TABLE context_revisions_new (
    id TEXT PRIMARY KEY NOT NULL,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    layer TEXT NOT NULL CHECK (layer IN ('global', 'harness', 'task', 'reported')),
    kind TEXT NOT NULL CHECK (kind IN (
        'global_policy', 'instructions', 'skill', 'settings', 'record', 'task_input', 'reported'
    )),
    source_path TEXT NOT NULL,
    source_revision TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    content TEXT,
    created_at TEXT NOT NULL
);

INSERT INTO context_revisions_new (
    id, execution_id, layer, kind, source_path, source_revision, content_hash, content, created_at
) SELECT id, execution_id, layer, kind, source_path, source_commit_sha, content_hash, content, created_at
FROM context_revisions;

DROP TABLE context_revisions;
DROP TABLE execution_events;
DROP TABLE executions;
DROP TABLE tasks;
ALTER TABLE tasks_new RENAME TO tasks;
ALTER TABLE executions_new RENAME TO executions;
ALTER TABLE execution_events_new RENAME TO execution_events;
ALTER TABLE context_revisions_new RENAME TO context_revisions;

CREATE INDEX execution_events_execution_id_id ON execution_events(execution_id, id);
CREATE INDEX context_revisions_execution_id ON context_revisions(execution_id);
CREATE INDEX task_workspaces_task_id ON task_workspaces(task_id);
CREATE INDEX stage_runs_execution_id ON stage_runs(execution_id);
CREATE INDEX check_results_execution_id ON check_results(execution_id);
