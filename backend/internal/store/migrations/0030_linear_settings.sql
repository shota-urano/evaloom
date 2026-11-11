-- Spec 02, sections 3.7 and 3.8. Keep existing transport history; add the
-- routing feedback kinds alongside the legacy kinds used by older callers.
CREATE TABLE linear_outbox_new (
    id TEXT PRIMARY KEY NOT NULL,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    linear_issue_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN (
        'intake_blocked', 'missing_acceptance_criteria', 'blocked', 'waiting_approval',
        'succeeded', 'failed', 'merged', 'cancelled', 'started', 'completed',
        'decision_open', 'waiting_decision', 'route_approved'
    )),
    action TEXT NOT NULL DEFAULT 'comment' CHECK (action IN ('comment', 'set_state', 'add_label')),
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
        'auth', 'issue_removed', 'retries_exhausted', 'state_not_found', 'label_not_found', 'other'
    )),
    failed_at TEXT,
    label_name TEXT,
    CHECK ((action = 'comment' AND state_name IS NULL AND label_name IS NULL AND body IS NOT NULL)
        OR (action = 'set_state' AND state_name IS NOT NULL AND label_name IS NULL AND body IS NULL
            AND outcome_unknown = 0 AND linear_comment_id IS NULL)
        OR (action = 'add_label' AND kind = 'route_approved' AND label_name IS NOT NULL
            AND state_name IS NULL AND body IS NULL AND outcome_unknown = 0 AND linear_comment_id IS NULL)),
    CHECK (kind <> 'route_approved' OR action = 'add_label'),
    CHECK (kind <> 'decision_open' OR action = 'comment'),
    CHECK (kind <> 'waiting_decision' OR action = 'set_state')
);

INSERT INTO linear_outbox_new (
    id, task_id, linear_issue_id, kind, action, state_name, dedupe_key, body, status,
    attempts, next_attempt_at, linear_comment_id, created_at, sent_at, outcome_unknown,
    execution_id, failure_reason, failed_at
) SELECT
    id, task_id, linear_issue_id, kind, action, state_name, dedupe_key, body, status,
    attempts, next_attempt_at, linear_comment_id, created_at, sent_at, outcome_unknown,
    execution_id, failure_reason, failed_at
FROM linear_outbox;

DROP TABLE linear_outbox;
ALTER TABLE linear_outbox_new RENAME TO linear_outbox;

CREATE TABLE settings_new (
    key TEXT PRIMARY KEY NOT NULL CHECK (key IN (
        'linear.teamId', 'linear.triggerLabel', 'linear.routingLabelPrefix',
        'routing.proposer', 'routing.defaultRunnerId', 'limits.stageTimeoutMaxSec', 'approval.ttlSec'
    )),
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
INSERT INTO settings_new SELECT key, value, updated_at FROM settings;
DROP TABLE settings;
ALTER TABLE settings_new RENAME TO settings;

-- Validate a label when it is enqueued or replaced. Do not make historical
-- deliveries a foreign-key owner of routes: routes may subsequently change.
-- Missing/malformed settings use the same trigger-label fallback as the app.
-- SQLite's default trim only removes ASCII spaces. Use Go strings.TrimSpace's
-- Unicode White_Space set for the blank check; keep nonblank labels verbatim.
CREATE TRIGGER linear_outbox_label_insert
BEFORE INSERT ON linear_outbox WHEN NEW.action = 'add_label' AND NEW.label_name IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'linear_outbox: label must be a route and must not be the trigger label')
    WHERE NOT EXISTS (SELECT 1 FROM routes WHERE label = NEW.label_name)
       OR NEW.label_name = coalesce((
           SELECT CASE WHEN json_valid(value) THEN
               CASE WHEN json_type(value) = 'text' AND trim(json_extract(value, '$'),
                   char(9, 10, 11, 12, 13, 32, 133, 160, 5760,
                        8192, 8193, 8194, 8195, 8196, 8197, 8198, 8199, 8200, 8201, 8202,
                        8232, 8233, 8239, 8287, 12288)) <> ''
                   THEN json_extract(value, '$') END
               END FROM settings WHERE key = 'linear.triggerLabel'
       ), 'ai-ready');
END;

CREATE TRIGGER linear_outbox_label_update
BEFORE UPDATE OF action, label_name ON linear_outbox WHEN NEW.action = 'add_label' AND NEW.label_name IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'linear_outbox: label must be a route and must not be the trigger label')
    WHERE NOT EXISTS (SELECT 1 FROM routes WHERE label = NEW.label_name)
       OR NEW.label_name = coalesce((
           SELECT CASE WHEN json_valid(value) THEN
               CASE WHEN json_type(value) = 'text' AND trim(json_extract(value, '$'),
                   char(9, 10, 11, 12, 13, 32, 133, 160, 5760,
                        8192, 8193, 8194, 8195, 8196, 8197, 8198, 8199, 8200, 8201, 8202,
                        8232, 8233, 8239, 8287, 12288)) <> ''
                   THEN json_extract(value, '$') END
               END FROM settings WHERE key = 'linear.triggerLabel'
       ), 'ai-ready');
END;
