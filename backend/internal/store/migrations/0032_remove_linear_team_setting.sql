-- Spec 02 3.8 and spec 15 3.2 (Q216, 2026-11-18): the polling Team is the
-- Linear Team whose key or name is uslab, resolved from Linear at startup.
-- linear.teamId is no longer a global setting: drop any stored row and
-- remove the key from the allowed keys. The outbox label triggers read
-- settings, so they are recreated after the table is rebuilt.
DROP TRIGGER linear_outbox_label_insert;
DROP TRIGGER linear_outbox_label_update;

CREATE TABLE settings_new (
    key TEXT PRIMARY KEY NOT NULL CHECK (key IN (
        'linear.triggerLabel', 'linear.routingLabelPrefix',
        'routing.proposer', 'routing.defaultRunnerId', 'limits.stageTimeoutMaxSec', 'approval.ttlSec'
    )),
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
INSERT INTO settings_new SELECT key, value, updated_at FROM settings WHERE key <> 'linear.teamId';
DROP TABLE settings;
ALTER TABLE settings_new RENAME TO settings;

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
