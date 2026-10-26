package api

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// NewHandler installs the owner API behind the process's authentication boundary.
func NewHandler(db *sql.DB, logger *slog.Logger, auth *OwnerAuth) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", auth.protect(newOwnerRoutes(db, logger)))
	mux.Handle("/auth/login", http.HandlerFunc(auth.login))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, logger, &Error{Code: NotFound, Message: "API endpoint not found"})
	}))
	return auth.Boundary(mux)
}

func newOwnerRoutes(db *sql.DB, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runners", func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			WriteError(w, logger, invalid("query", "invalid"))
			return
		}
		page, err := ParsePage(query, "runners")
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		rows, err := store.ListRunners(r.Context(), db, page.After, page.Limit+1)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		items := make([]runnerResponse, 0, len(rows))
		for _, row := range rows {
			heartbeat, err := ParseTimestamp(row.LastHeartbeatAt)
			if err != nil {
				WriteError(w, logger, fmt.Errorf("runner %q: invalid stored heartbeat", row.ID))
				return
			}
			created, err := ParseTimestamp(row.CreatedAt)
			if err != nil {
				WriteError(w, logger, fmt.Errorf("runner %q: invalid stored creation time", row.ID))
				return
			}
			items = append(items, runnerResponse{row.ID, row.Name, row.Status, heartbeat, created})
		}
		writeJSON(w, http.StatusOK, NewPage(items, page.Limit, "runners", func(r runnerResponse) string { return r.ID }))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, logger, &Error{Code: NotFound, Message: "API endpoint not found"})
	})
	return mux
}

type runnerResponse struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Status          string    `json:"status"`
	LastHeartbeatAt Timestamp `json:"lastHeartbeatAt"`
	CreatedAt       Timestamp `json:"createdAt"`
}
