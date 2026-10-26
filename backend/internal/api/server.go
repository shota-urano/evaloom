package api

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// NewHandler routes each API through its own authentication class without
// serving the Management UI.
// Supply the Runner's Adapter Registry to enable Harness registration. With no
// Registry, no Adapter names are accepted; concrete Adapters are separate work.
func NewHandler(db *sql.DB, logger *slog.Logger, auth *OwnerAuth, registries ...*adapter.Registry) http.Handler {
	return NewHandlerWithWeb(db, logger, auth, nil, registries...)
}

// NewHandlerWithWeb additionally serves the built Management UI from web on the
// same origin (architecture 3.1). A nil web keeps unknown routes as 404.
func NewHandlerWithWeb(db *sql.DB, logger *slog.Logger, auth *OwnerAuth, web fs.FS, registries ...*adapter.Registry) http.Handler {
	adapters := &adapter.Registry{}
	if len(registries) > 0 && registries[0] != nil {
		adapters = registries[0]
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", auth.Protect(newOwnerRoutes(db, logger, adapters)))
	mux.Handle("/auth/login", auth.Boundary(http.HandlerFunc(auth.login)))
	mux.Handle("POST /api/runners/{runnerID}/claim", auth.protectRunner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Authentication probe only: claiming work belongs to the task lifecycle.
		WriteError(w, logger, &Error{Code: PreconditionFailed, Message: "Runner claim is not available"})
	})))
	// Reserve the remaining declared Runner routes in the correct auth class.
	// They perform no heartbeat, event, result, or context processing yet.
	for _, pattern := range []string{
		"POST /api/runners/{runnerID}/heartbeat",
		"POST /api/executions/{id}/events",
		"POST /api/executions/{id}/result",
		"POST /api/executions/{id}/context",
	} {
		mux.Handle(pattern, auth.protectRunner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			WriteError(w, logger, &Error{Code: NotFound, Message: "API endpoint not found"})
		})))
	}
	fallback := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, logger, &Error{Code: NotFound, Message: "API endpoint not found"})
	}))
	if web != nil {
		fallback = webHandler(web, logger)
	}
	mux.Handle("/", auth.Boundary(fallback))
	return auth.boundary(mux, false)
}

func newOwnerRoutes(db *sql.DB, logger *slog.Logger, adapters *adapter.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/projects", listProjects(db, logger))
	mux.HandleFunc("POST /api/projects", writeProject(db, logger, true))
	mux.HandleFunc("GET /api/projects/{id}", getProject(db, logger))
	mux.HandleFunc("PATCH /api/projects/{id}", writeProject(db, logger, false))
	mux.HandleFunc("GET /api/harnesses", listHarnesses(db, logger))
	mux.HandleFunc("POST /api/harnesses", writeHarness(db, logger, adapters, true))
	mux.HandleFunc("PATCH /api/harnesses/{id}", writeHarness(db, logger, adapters, false))
	mux.HandleFunc("GET /api/harness-profiles", listHarnessProfiles(db, logger))
	mux.HandleFunc("POST /api/harness-profiles", registerHarnessProfile(db, logger))
	mux.HandleFunc("GET /api/executions/{id}/events", executionEvents(db, logger))
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
			var heartbeat *Timestamp
			if row.LastHeartbeatAt != "" {
				parsed, err := ParseTimestamp(row.LastHeartbeatAt)
				if err != nil {
					WriteError(w, logger, fmt.Errorf("runner %q: invalid stored heartbeat", row.ID))
					return
				}
				heartbeat = &parsed
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
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Status          string     `json:"status"`
	LastHeartbeatAt *Timestamp `json:"lastHeartbeatAt"`
	CreatedAt       Timestamp  `json:"createdAt"`
}
