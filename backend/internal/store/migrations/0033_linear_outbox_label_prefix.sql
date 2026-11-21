-- Spec 16 3.6.5: only routing labels may be enqueued. Keep the existing
-- route membership and trigger-label guards. Match settings' JSON string
-- and Unicode whitespace fallback rules; substr is case-sensitive and
-- treats LIKE wildcard characters literally.
CREATE TRIGGER linear_outbox_label_prefix_insert
BEFORE INSERT ON linear_outbox WHEN NEW.action = 'add_label'
BEGIN
    SELECT RAISE(ABORT, 'linear_outbox: label must start with the routing prefix')
    FROM (SELECT coalesce((
        SELECT CASE WHEN json_valid(value) THEN
            CASE WHEN json_type(value) = 'text' AND trim(json_extract(value, '$'),
                char(9, 10, 11, 12, 13, 32, 133, 160, 5760,
                     8192, 8193, 8194, 8195, 8196, 8197, 8198, 8199, 8200, 8201, 8202,
                     8232, 8233, 8239, 8287, 12288)) <> ''
                THEN json_extract(value, '$') END
            END FROM settings WHERE key = 'linear.routingLabelPrefix'
    ), 'route:') AS prefix)
    WHERE substr(NEW.label_name, 1, length(prefix)) <> prefix;
END;

CREATE TRIGGER linear_outbox_label_prefix_update
BEFORE UPDATE OF action, label_name ON linear_outbox WHEN NEW.action = 'add_label'
BEGIN
    SELECT RAISE(ABORT, 'linear_outbox: label must start with the routing prefix')
    FROM (SELECT coalesce((
        SELECT CASE WHEN json_valid(value) THEN
            CASE WHEN json_type(value) = 'text' AND trim(json_extract(value, '$'),
                char(9, 10, 11, 12, 13, 32, 133, 160, 5760,
                     8192, 8193, 8194, 8195, 8196, 8197, 8198, 8199, 8200, 8201, 8202,
                     8232, 8233, 8239, 8287, 12288)) <> ''
                THEN json_extract(value, '$') END
            END FROM settings WHERE key = 'linear.routingLabelPrefix'
    ), 'route:') AS prefix)
    WHERE substr(NEW.label_name, 1, length(prefix)) <> prefix;
END;
