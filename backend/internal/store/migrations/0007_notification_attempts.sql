-- Reserve delivery before invoking any external Notifier. A process crash or
-- failed delivery must not cause a second attempt after restart.
ALTER TABLE notification_requests ADD COLUMN attempted_at TEXT;

CREATE INDEX notification_requests_pending
    ON notification_requests(created_at, execution_id, status)
    WHERE attempted_at IS NULL;
