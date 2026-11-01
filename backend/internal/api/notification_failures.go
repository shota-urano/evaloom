package api

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

type notificationFailureResponse struct {
	EventID     int64     `json:"eventId"`
	ExecutionID string    `json:"executionId"`
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
		var before int64
		if page.After != "" {
			before, err = strconv.ParseInt(page.After, 10, 64)
			if err != nil || before <= 0 || strings.Trim(page.After, "0123456789") != "" {
				WriteError(w, logger, invalid("cursor", "invalid"))
				return
			}
		}
		rows, err := store.ListNotificationFailures(r.Context(), db, before, page.Limit+1)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		items := make([]notificationFailureResponse, 0, len(rows))
		for _, row := range rows {
			created, err := ParseTimestamp(row.CreatedAt)
			if err != nil {
				WriteError(w, logger, fmt.Errorf("notification failure %d: invalid stored creation time", row.EventID))
				return
			}
			items = append(items, notificationFailureResponse{row.EventID, row.ExecutionID, row.TaskID,
				row.TaskTitle, row.Kind, row.Channel, row.Reason, created})
		}
		result := NewPage(items, page.Limit, resource, func(item notificationFailureResponse) string {
			return strconv.FormatInt(item.EventID, 10)
		})
		response := notificationFailurePage{Items: result.Items}
		// Q41 explicitly uses null at the end, unlike older list endpoints.
		if result.NextCursor != "" {
			response.NextCursor = &result.NextCursor
		}
		writeJSON(w, http.StatusOK, response)
	}
}
