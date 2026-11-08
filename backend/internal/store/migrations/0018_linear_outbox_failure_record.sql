-- docs/specs/16-linear-feedback.md 3.4.2, 3.7 (Q82, Q85, decided 2026-11-08).
-- execution_id is the Execution of the trigger; it is NULL for triggers
-- without one (intake_blocked, missing_acceptance_criteria and the
-- cancellation of a held task). Failed rows with a NULL execution_id are
-- the failure record GET /api/notification-failures returns (spec 12 5章).
-- failure_reason is the notification_failed reason for channel=linear and
-- failed_at the time the row was marked failed.
ALTER TABLE linear_outbox ADD COLUMN execution_id TEXT REFERENCES executions(id);
ALTER TABLE linear_outbox ADD COLUMN failure_reason TEXT
    CHECK (failure_reason IS NULL OR failure_reason IN ('auth', 'issue_removed', 'retries_exhausted', 'other'));
ALTER TABLE linear_outbox ADD COLUMN failed_at TEXT;

-- Existing rows: execution_id is the dedupe target (<task_id>:<kind>:<id>)
-- only when it is an Execution of the row's task.
UPDATE linear_outbox SET execution_id = (
    SELECT e.id FROM executions e
    WHERE e.task_id = linear_outbox.task_id
      AND linear_outbox.dedupe_key = linear_outbox.task_id || ':' || linear_outbox.kind || ':' || e.id
);
-- Failed rows have no record of when or why they failed.
UPDATE linear_outbox SET failure_reason = 'other', failed_at = created_at WHERE status = 'failed';
