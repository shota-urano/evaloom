-- Spec 02, sections 3.4 and 5.6. Follow 5.6 for question's cause;
-- section 3.4's prose still says non-blocked causes are NULL.
CREATE TABLE owner_decisions (
    id TEXT PRIMARY KEY NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('route', 'approval', 'blocked', 'question')),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    execution_id TEXT REFERENCES executions(id),
    subject_id TEXT,
    cause TEXT,
    basis TEXT,
    detail TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('open', 'answered', 'withdrawn')),
    withdrawn_reason TEXT CHECK (withdrawn_reason IN ('cancelled', 'superseded', 'expired')),
    answer TEXT,
    answered_at TEXT,
    created_at TEXT NOT NULL,
    CHECK (
        (kind IN ('route', 'approval') AND cause IS NULL) OR
        (kind = 'blocked' AND cause IS NOT NULL AND cause IN (
            'check_blocked', 'publish_unknown', 'context_conflict', 'workspace_missing'
        )) OR
        (kind = 'question' AND cause IS NOT NULL AND cause = 'question')
    ),
    CHECK ((kind = 'route' AND execution_id IS NULL) OR
           (kind <> 'route' AND execution_id IS NOT NULL)),
    CHECK ((kind IN ('route', 'approval') AND subject_id IS NOT NULL) OR
           (kind IN ('blocked', 'question') AND subject_id IS NULL))
);

CREATE UNIQUE INDEX owner_decisions_open_task
    ON owner_decisions(task_id) WHERE status = 'open' AND kind <> 'approval';
CREATE UNIQUE INDEX owner_decisions_open_execution_subject
    ON owner_decisions(execution_id, subject_id) WHERE status = 'open' AND kind = 'approval';

CREATE TABLE route_proposals (
    id TEXT PRIMARY KEY NOT NULL,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    trigger TEXT NOT NULL CHECK (trigger IN ('no_label', 'unknown_label', 'multiple_labels')),
    seen_labels TEXT NOT NULL,
    candidates TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN (
        'queued', 'running', 'proposed', 'failed', 'decided', 'superseded', 'cancelled'
    )),
    runner_id TEXT REFERENCES runners(id),
    proposed_label TEXT,
    reason TEXT CHECK (length(reason) <= 2000),
    error_class TEXT CHECK (error_class IN ('sandbox_error', 'timeout', 'contract_error', 'interrupted')),
    decision_id TEXT REFERENCES owner_decisions(id),
    created_at TEXT NOT NULL,
    finished_at TEXT
);

CREATE TABLE approvals_new (
    id TEXT PRIMARY KEY NOT NULL,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    execution_id TEXT NOT NULL REFERENCES executions(id),
    decision_id TEXT NOT NULL REFERENCES owner_decisions(id),
    requested_stage TEXT NOT NULL CHECK (requested_stage IN ('check', 'publish')),
    operation TEXT NOT NULL,
    target TEXT NOT NULL,
    summary TEXT,
    content_id TEXT NOT NULL,
    params_hash TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN (
        'pending', 'approved', 'rejected', 'consumed', 'invalidated', 'expired'
    )),
    invalidated_reason TEXT CHECK (invalidated_reason IN ('content_changed', 'superseded', 'cancelled')),
    decided_at TEXT,
    expires_at TEXT,
    max_uses INTEGER NOT NULL DEFAULT 1 CHECK (max_uses = 1),
    use_count INTEGER NOT NULL DEFAULT 0 CHECK (use_count >= 0 AND use_count <= max_uses),
    consumed_at TEXT,
    created_at TEXT NOT NULL
);

-- Legacy approvals have neither a decision nor a requested stage. Reject a
-- nonempty legacy table atomically instead of inventing that missing history.
INSERT INTO approvals_new (
    id, task_id, execution_id, decision_id, requested_stage, operation, target,
    content_id, params_hash, status, invalidated_reason, decided_at, expires_at,
    max_uses, use_count, consumed_at, created_at
) SELECT id, task_id, execution_id, NULL, NULL, operation, target,
    content_version, params_hash, status, invalidated_reason, decided_at, expires_at,
    max_uses, use_count, consumed_at, created_at
FROM approvals;

DROP TABLE approvals;
ALTER TABLE approvals_new RENAME TO approvals;
CREATE UNIQUE INDEX approvals_open_execution_operation
    ON approvals(execution_id, operation) WHERE status IN ('pending', 'approved');
