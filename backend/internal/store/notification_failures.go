package store

import (
	"context"
	"database/sql"
)

// NotificationFailure projects only the public notification fields, never an
// Execution's error_message or any additional event payload fields.
type NotificationFailure struct {
	EventID     int64
	ExecutionID string
	TaskID      string
	TaskTitle   string
	Kind        string
	Channel     string
	Reason      string
	CreatedAt   string
}

// ListNotificationFailures includes all Executions, including earlier retry
// attempts. The exclusive global event ID boundary is independent of timestamps.
func ListNotificationFailures(ctx context.Context, db *sql.DB, before int64, limit int) ([]NotificationFailure, error) {
	rows, err := db.QueryContext(ctx, `SELECT v.id, v.execution_id, t.id, t.title,
		json_extract(v.payload, '$.kind'), json_extract(v.payload, '$.channel'),
		json_extract(v.payload, '$.reason'), v.created_at
		FROM execution_events v JOIN executions e ON e.id = v.execution_id JOIN tasks t ON t.id = e.task_id
		WHERE v.type = 'notification_failed' AND (? = 0 OR v.id < ?)
		ORDER BY v.id DESC LIMIT ?`, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []NotificationFailure{}
	for rows.Next() {
		var item NotificationFailure
		if err := rows.Scan(&item.EventID, &item.ExecutionID, &item.TaskID, &item.TaskTitle,
			&item.Kind, &item.Channel, &item.Reason, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
