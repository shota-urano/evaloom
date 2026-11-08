package store

import (
	"context"
	"database/sql"
)

// Notification failure sources, also the tie-break of the list order
// (spec 12 5章: within one time, by source and ID).
const (
	FailureSourceEvent  = "e" // execution_events.type = notification_failed
	FailureSourceOutbox = "o" // linear_outbox, status=failed and execution_id NULL
)

// NotificationFailure projects only the public notification fields, never an
// Execution's error_message, an additional event payload field or a comment
// body. Exactly one of EventID / OutboxID is set; ExecutionID is set for
// events only (spec 12 5章, Q82).
type NotificationFailure struct {
	EventID     *int64
	ExecutionID *string
	OutboxID    *string
	TaskID      string
	TaskTitle   string
	Kind        string
	Channel     string
	Reason      string
	CreatedAt   string
	// Position is the item's place in the list order; the next page starts
	// after it.
	Position NotificationFailurePosition
}

// NotificationFailurePosition is a place in the list order: the recorded
// time (execution_events.created_at or linear_outbox.failed_at) normalized
// by SQLite to fixed-width milliseconds, then the source, then the ID within
// the source. The list is newest first, so a later page holds positions
// strictly smaller.
type NotificationFailurePosition struct {
	Time     string // YYYY-MM-DDTHH:MM:SS.SSSZ
	Source   string // FailureSourceEvent or FailureSourceOutbox
	EventID  int64  // for FailureSourceEvent
	OutboxID string // for FailureSourceOutbox
}

// ListNotificationFailures returns the notification_failed events of all
// Executions (including earlier retry attempts) merged with the failed
// linear_outbox rows that have no Execution (spec 12 5章, spec 16 3.4.2).
// A Linear failure with an Execution is only listed as its event. The order
// is the recorded time, newest first, then source and ID; after, when not
// nil, is the exclusive boundary of the previous page.
//
// Both sources store RFC 3339 UTC text of different precision, so the times
// are compared after strftime brings them to one fixed width. An invalid
// stored time becomes NULL and fails when scanned.
func ListNotificationFailures(ctx context.Context, db *sql.DB, after *NotificationFailurePosition, limit int) ([]NotificationFailure, error) {
	var cursorTime, cursorSource, cursorID any
	if after != nil {
		cursorTime, cursorSource = after.Time, after.Source
		if after.Source == FailureSourceEvent {
			cursorID = after.EventID
		} else {
			cursorID = after.OutboxID
		}
	}
	// sid is an INTEGER for events and TEXT for outbox rows; it is only
	// compared within one source.
	rows, err := db.QueryContext(ctx, `SELECT pos, src, event_id, execution_id, outbox_id, task_id, title,
		kind, channel, reason, recorded_at FROM (
			SELECT strftime('%Y-%m-%dT%H:%M:%fZ', v.created_at) AS pos, 'e' AS src, v.id AS sid,
				v.id AS event_id, v.execution_id AS execution_id, NULL AS outbox_id, t.id AS task_id, t.title AS title,
				json_extract(v.payload, '$.kind') AS kind, json_extract(v.payload, '$.channel') AS channel,
				json_extract(v.payload, '$.reason') AS reason, v.created_at AS recorded_at
			FROM execution_events v JOIN executions e ON e.id = v.execution_id JOIN tasks t ON t.id = e.task_id
			WHERE v.type = 'notification_failed'
			UNION ALL
			SELECT strftime('%Y-%m-%dT%H:%M:%fZ', o.failed_at), 'o', o.id,
				NULL, NULL, o.id, t.id, t.title, o.kind, 'linear', o.failure_reason, o.failed_at
			FROM linear_outbox o JOIN tasks t ON t.id = o.task_id
			WHERE o.status = 'failed' AND o.execution_id IS NULL
		)
		WHERE ?1 IS NULL OR pos < ?1 OR (pos = ?1 AND (src < ?2 OR (src = ?2 AND sid < ?3)))
		ORDER BY pos DESC, src DESC, sid DESC LIMIT ?4`, cursorTime, cursorSource, cursorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []NotificationFailure{}
	for rows.Next() {
		var item NotificationFailure
		var eventID sql.NullInt64
		var executionID, outboxID sql.NullString
		if err := rows.Scan(&item.Position.Time, &item.Position.Source, &eventID, &executionID, &outboxID,
			&item.TaskID, &item.TaskTitle, &item.Kind, &item.Channel, &item.Reason, &item.CreatedAt); err != nil {
			return nil, err
		}
		if eventID.Valid {
			item.EventID, item.Position.EventID = &eventID.Int64, eventID.Int64
		}
		if executionID.Valid {
			item.ExecutionID = &executionID.String
		}
		if outboxID.Valid {
			item.OutboxID, item.Position.OutboxID = &outboxID.String, outboxID.String
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
