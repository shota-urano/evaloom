package api

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// notificationFailureResponse is one item of GET /api/notification-failures
// (spec 12 5章). Items from execution_events carry eventId and executionId;
// items from linear_outbox (Linear failures without an Execution, spec 16
// 3.4.2, Q82) carry outboxId instead. Error text and comment bodies are
// never part of an item.
type notificationFailureResponse struct {
	EventID     *int64    `json:"eventId"`
	ExecutionID *string   `json:"executionId"`
	OutboxID    *string   `json:"outboxId"`
	TaskID      string    `json:"taskId"`
	TaskTitle   string    `json:"taskTitle"`
	Kind        string    `json:"kind"`
	Channel     string    `json:"channel"`
	Reason      string    `json:"reason"`
	CreatedAt   Timestamp `json:"createdAt"`
}

type notificationFailurePage struct {
	Items      []notificationFailureResponse `json:"items"`
	NextCursor *string                       `json:"nextCursor"`
}

// notificationFailureCursorTime is the fixed-width time of a list position
// (store.NotificationFailurePosition.Time).
var notificationFailureCursorTime = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$`)

const notificationFailureTimeLayout = "2006-01-02T15:04:05.000Z"

// formatNotificationFailureCursor encodes a list position as
// "<time>|<source>|<id>" (inside the opaque resource-bound token).
func formatNotificationFailureCursor(p store.NotificationFailurePosition) string {
	id := p.OutboxID
	if p.Source == store.FailureSourceEvent {
		id = strconv.FormatInt(p.EventID, 10)
	}
	return p.Time + "|" + p.Source + "|" + id
}

func parseNotificationFailureCursor(value string) (*store.NotificationFailurePosition, error) {
	parts := strings.SplitN(value, "|", 3)
	if len(parts) != 3 || !notificationFailureCursorTime.MatchString(parts[0]) || parts[2] == "" {
		return nil, invalid("cursor", "invalid")
	}
	if _, err := time.Parse(notificationFailureTimeLayout, parts[0]); err != nil {
		return nil, invalid("cursor", "invalid")
	}
	p := &store.NotificationFailurePosition{Time: parts[0], Source: parts[1]}
	switch parts[1] {
	case store.FailureSourceEvent:
		id, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || id <= 0 || strings.Trim(parts[2], "0123456789") != "" {
			return nil, invalid("cursor", "invalid")
		}
		p.EventID = id
	case store.FailureSourceOutbox:
		p.OutboxID = parts[2]
	default:
		return nil, invalid("cursor", "invalid")
	}
	return p, nil
}

func listNotificationFailures(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			WriteError(w, logger, invalid("query", "invalid"))
			return
		}
		const resource = "notification-failures"
		page, err := ParsePage(query, resource)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		var after *store.NotificationFailurePosition
		if page.After != "" {
			if after, err = parseNotificationFailureCursor(page.After); err != nil {
				WriteError(w, logger, err)
				return
			}
		}
		rows, err := store.ListNotificationFailures(r.Context(), db, after, page.Limit+1)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		items := make([]notificationFailureResponse, 0, len(rows))
		for _, row := range rows {
			created, err := ParseTimestamp(row.CreatedAt)
			if err != nil {
				WriteError(w, logger, fmt.Errorf("notification failure %s: invalid stored time",
					formatNotificationFailureCursor(row.Position)))
				return
			}
			items = append(items, notificationFailureResponse{row.EventID, row.ExecutionID, row.OutboxID,
				row.TaskID, row.TaskTitle, row.Kind, row.Channel, row.Reason, created})
		}
		// The cursor is the position of the last item in the list order
		// (spec 12 5章, Q82), so pages across both sources neither skip nor
		// repeat an item.
		var nextCursor string
		if len(rows) > page.Limit {
			nextCursor = formatNotificationFailureCursor(rows[page.Limit-1].Position)
		}
		result := NewPage(items, page.Limit, resource, func(notificationFailureResponse) string { return nextCursor })
		response := notificationFailurePage{Items: result.Items}
		// Q41 explicitly uses null at the end, unlike older list endpoints.
		if result.NextCursor != "" {
			response.NextCursor = &result.NextCursor
		}
		writeJSON(w, http.StatusOK, response)
	}
}
