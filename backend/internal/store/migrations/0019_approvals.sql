-- docs/specs/17-approval.md 3.9 (Phase 2 approvals).
-- Rebuild executions to add 'waiting_approval' and 'merging' to the status
-- CHECK. Columns and values are copied unchanged. The migration runner
-- disables foreign keys for the rebuild and runs foreign_key_check before
-- commit, so references from other tables to executions(id) stay valid.
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
    worktree_id TEXT REFERENCES worktrees(id),
    branch_name TEXT,
    base_sha TEXT,
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

INSERT INTO executions_new (
    id, task_id, seq, mode, status, runner_id, role, harness_id, profile_id,
    profile_version, profile_commit_sha, worktree_id, branch_name, base_sha,
    resume_point, harness_head_sha, verified_sha, pushed_sha, pr_number, pr_url,
    harness_session_id, error_class, error_message, cancel_requested_at,
    claimed_at, started_at, finished_at, last_heartbeat_at, created_at
) SELECT
    id, task_id, seq, mode, status, runner_id, role, harness_id, profile_id,
    profile_version, profile_commit_sha, worktree_id, branch_name, base_sha,
    resume_point, harness_head_sha, verified_sha, pushed_sha, pr_number, pr_url,
    harness_session_id, error_class, error_message, cancel_requested_at,
    claimed_at, started_at, finished_at, last_heartbeat_at, created_at
FROM executions;

DROP TABLE executions;
ALTER TABLE executions_new RENAME TO executions;

-- operation lists every 3.1 value up front so later operations do not
-- require rebuilding this table; Phase 2 writes only merge_pull_request.
CREATE TABLE approvals (
    id TEXT PRIMARY KEY NOT NULL,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    execution_id TEXT NOT NULL REFERENCES executions(id),
    operation TEXT NOT NULL CHECK (operation IN (
        'merge_pull_request', 'deploy_production', 'send_external_email',
        'delete_important_file', 'change_auth', 'change_contract',
        'change_evaluation_criteria', 'change_security_policy'
    )),
    target TEXT NOT NULL CHECK (json_valid(target)),
    content_version TEXT NOT NULL,
    params_hash TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN (
        'pending', 'approved', 'rejected', 'consumed', 'invalidated', 'expired'
    )),
    invalidated_reason TEXT CHECK (invalidated_reason IN (
        'content_changed', 'target_changed', 'superseded', 'cancelled'
    )),
    decided_by TEXT CHECK (decided_by = 'owner'),
    decided_at TEXT,
    expires_at TEXT,
    max_uses INTEGER NOT NULL DEFAULT 1 CHECK (max_uses >= 1),
    use_count INTEGER NOT NULL DEFAULT 0 CHECK (use_count >= 0 AND use_count <= max_uses),
    consumed_at TEXT,
    result TEXT CHECK (result IS NULL OR json_valid(result)),
    created_at TEXT NOT NULL
);

-- At most one unused approval per Execution and operation (3.9).
CREATE UNIQUE INDEX approvals_open_execution_operation
    ON approvals(execution_id, operation) WHERE status IN ('pending', 'approved');
