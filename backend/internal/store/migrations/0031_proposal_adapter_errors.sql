-- Spec 02 route_proposals and spec 08 3.10 (2026-11-17): report Adapter
-- configuration and CLI failures without leaving a running proposal behind.
CREATE TABLE route_proposals_new (
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
    error_class TEXT CHECK (error_class IN (
        'sandbox_error', 'timeout', 'contract_error', 'config_error', 'work_failed', 'interrupted'
    )),
    decision_id TEXT REFERENCES owner_decisions(id),
    created_at TEXT NOT NULL,
    finished_at TEXT
);

INSERT INTO route_proposals_new (
    id, task_id, trigger, seen_labels, candidates, status, runner_id,
    proposed_label, reason, error_class, decision_id, created_at, finished_at
) SELECT
    id, task_id, trigger, seen_labels, candidates, status, runner_id,
    proposed_label, reason, error_class, decision_id, created_at, finished_at
FROM route_proposals;

DROP TABLE route_proposals;
ALTER TABLE route_proposals_new RENAME TO route_proposals;
