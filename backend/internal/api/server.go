package api

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
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
	return NewHandlerWithArtifacts(db, logger, auth, web, ArtifactConfig{}, registries...)
}

// NewHandlerWithArtifacts uses only Control Plane supplied storage and limits.
// The Linear label settings are read from and written to db's settings table.
func NewHandlerWithArtifacts(db *sql.DB, logger *slog.Logger, auth *OwnerAuth, web fs.FS, artifacts ArtifactConfig, registries ...*adapter.Registry) http.Handler {
	return NewHandlerWithSettings(db, logger, auth, web, artifacts, linearintake.NewLabelSettings(db, logger), registries...)
}

// NewHandlerWithSettings serves GET/PATCH /api/settings from settings, which
// the Control Plane shares with its Linear Poller (spec 15 5章).
func NewHandlerWithSettings(db *sql.DB, logger *slog.Logger, auth *OwnerAuth, web fs.FS, artifacts ArtifactConfig, settings LinearSettings, registries ...*adapter.Registry) http.Handler {
	adapters := &adapter.Registry{}
	if len(registries) > 0 && registries[0] != nil {
		adapters = registries[0]
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", auth.Protect(newOwnerRoutes(db, logger, adapters, artifacts, settings)))
	mux.Handle("/auth/login", auth.Boundary(http.HandlerFunc(auth.login)))
	mux.Handle("POST /auth/logout", auth.Protect(http.HandlerFunc(auth.logout)))
	mux.Handle("POST /api/runners/{runnerID}/claim", auth.protectRunner(claimExecution(db, logger)))
	mux.Handle("POST /api/executions/{id}/result", auth.protectRunner(reportExecutionResult(db, logger, auth)))
	mux.Handle("POST /api/executions/{id}/authorize", auth.protectRunner(authorizeExecution(db, logger, auth)))
	mux.Handle("POST /api/runners/{runnerID}/heartbeat", auth.protectRunner(runnerHeartbeat(db, logger, auth)))
	mux.Handle("POST /api/executions/{id}/events", auth.protectRunner(reportLogEvents(db, logger, auth)))
	mux.Handle("POST /api/executions/{id}/artifacts", auth.protectRunner(registerArtifact(db, logger, auth, artifacts)))
	fallback := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, logger, &Error{Code: NotFound, Message: "API endpoint not found"})
	}))
	if web != nil {
		fallback = webHandler(web, logger)
	}
	mux.Handle("/", auth.Boundary(fallback))
	return auth.boundary(mux, false)
}

func newOwnerRoutes(db *sql.DB, logger *slog.Logger, adapters *adapter.Registry, artifacts ArtifactConfig, settings LinearSettings) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/settings", getSettings(settings))
	mux.HandleFunc("PATCH /api/settings", patchSettings(settings, logger))
	mux.HandleFunc("GET /api/linear/status", getLinearStatus(db, logger))
	mux.HandleFunc("GET /api/tasks", listTasks(db, logger))
	mux.HandleFunc("GET /api/notification-failures", listNotificationFailures(db, logger))
	mux.HandleFunc("POST /api/tasks", createTask(db, logger))
	mux.HandleFunc("GET /api/tasks/{id}", getTask(db, logger))
	mux.HandleFunc("POST /api/tasks/{id}/cancel", cancelTask(db, logger))
	mux.HandleFunc("POST /api/tasks/{id}/retry", retryTask(db, logger))
	mux.HandleFunc("GET /api/approvals", listApprovals(db, logger))
	mux.HandleFunc("GET /api/approvals/{id}", getApproval(db, logger))
	mux.HandleFunc("GET /api/decisions", listDecisions(db, logger))
	mux.HandleFunc("GET /api/decisions/{id}", getDecision(db, logger))
	mux.HandleFunc("POST /api/decisions/{id}/answer", answerDecision(db, logger, existingApprovalAnswers{}))
	mux.HandleFunc("GET /api/routes", listRoutes(db, logger))
	mux.HandleFunc("POST /api/routes", writeRoute(db, logger, true))
	mux.HandleFunc("PATCH /api/routes/{label}", writeRoute(db, logger, false))
	mux.HandleFunc("DELETE /api/routes/{label}", deleteRoute(db, logger))
	mux.HandleFunc("POST /api/approvals/{id}/renew", renewApproval(db, logger))
	mux.HandleFunc("GET /api/projects", listProjects(db, logger))
	mux.HandleFunc("POST /api/projects", writeProject(db, logger, true))
	mux.HandleFunc("GET /api/projects/{id}", getProject(db, logger))
	mux.HandleFunc("PATCH /api/projects/{id}", writeProject(db, logger, false))
	mux.HandleFunc("GET /api/harnesses", listHarnesses(db, logger))
	mux.HandleFunc("GET /api/harnesses/{id}", getHarness(db, logger))
	mux.HandleFunc("POST /api/harnesses", writeHarness(db, logger, adapters, true))
	mux.HandleFunc("PATCH /api/harnesses/{id}", writeHarness(db, logger, adapters, false))
	mux.HandleFunc("GET /api/harness-profiles", listHarnessProfiles(db, logger))
	mux.HandleFunc("GET /api/harness-profiles/{id}", getHarnessProfile(db, logger))
	mux.HandleFunc("POST /api/harness-profiles", registerHarnessProfile(db, logger, adapters))
	mux.HandleFunc("GET /api/executions/{id}", getExecution(db, logger))
	mux.HandleFunc("GET /api/executions/{id}/events", executionEvents(db, logger))
	mux.HandleFunc("GET /api/executions/{id}/logs", executionLogs(db, logger, artifacts))
	mux.HandleFunc("GET /api/executions/{id}/artifacts/{artifactId}", getArtifact(logger, artifacts))
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
			if row.LastHeartbeatAt != nil {
				parsed, err := ParseTimestamp(*row.LastHeartbeatAt)
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
