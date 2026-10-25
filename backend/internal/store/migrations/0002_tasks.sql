CREATE TABLE tasks (
    id TEXT PRIMARY KEY NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id),
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
    updated_at TEXT NOT NULL
);

CREATE TABLE worktrees (
    id TEXT PRIMARY KEY NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    runner_id TEXT NOT NULL REFERENCES runners(id),
    path TEXT NOT NULL,
    branch_name TEXT NOT NULL,
    base_sha TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('active', 'retained', 'cleaned')),
    setup_status TEXT NOT NULL CHECK (setup_status IN ('pending', 'succeeded', 'failed')),
    copied_files TEXT NOT NULL,
    created_at TEXT NOT NULL,
    cleaned_at TEXT
);

CREATE TABLE executions (
    id TEXT PRIMARY KEY NOT NULL,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    seq INTEGER NOT NULL CHECK (seq >= 1),
    mode TEXT NOT NULL CHECK (mode IN ('continue', 'fresh')),
    status TEXT NOT NULL CHECK (status IN (
        'pending', 'claimed', 'preparing', 'running', 'verifying', 'publishing',
        'succeeded', 'blocked', 'failed', 'interrupted', 'cancelled', 'timed_out'
    )),
    runner_id TEXT REFERENCES runners(id),
    role TEXT,
    harness_id TEXT REFERENCES harnesses(id),
    profile_id TEXT REFERENCES harness_profiles(id),
    profile_version TEXT,
    profile_commit_sha TEXT,
    worktree_id TEXT REFERENCES worktrees(id),
    branch_name TEXT,
    base_sha TEXT,
    gate1_base_sha TEXT,
    resume_point TEXT CHECK (resume_point IN ('gate2', 'gate3', 'publish')),
    harness_head_sha TEXT,
    verified_sha TEXT,
    pushed_sha TEXT,
    pr_number INTEGER,
    pr_url TEXT,
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

CREATE TABLE execution_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    type TEXT NOT NULL CHECK (type IN (
        'status_changed', 'log', 'worktree_ready', 'context_pinned',
        'harness_started', 'harness_finished', 'gate_result', 'blocked',
        'pushed', 'pr_created', 'notification_sent'
    )),
    payload TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX execution_events_execution_id_id ON execution_events(execution_id, id);

CREATE TABLE context_revisions (
    id TEXT PRIMARY KEY NOT NULL,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    layer TEXT NOT NULL CHECK (layer IN ('global', 'harness', 'project', 'task')),
    kind TEXT NOT NULL,
    source_path TEXT NOT NULL,
    source_commit_sha TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    content TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX context_revisions_execution_id ON context_revisions(execution_id);
