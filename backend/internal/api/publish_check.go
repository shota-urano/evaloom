package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// publishCheckResponse answers the push-time questions of spec 17 3.7.1:
// whether the task has a cancellation request (step 1) and the current
// approval policy (step 2), in the 3.2 shape.
type publishCheckResponse struct {
	CancelRequested bool               `json:"cancelRequested"`
	ApprovalPolicy  publishCheckPolicy `json:"approvalPolicy"`
}

type publishCheckPolicy struct {
	Version          int    `json:"version"`
	MergePullRequest string `json:"mergePullRequest"`
}

// publishCheck is POST /api/executions/{id}/publish-check (spec 17 5章).
// Only the Runner that runs the Execution may call it, and only while the
// Execution is the task's latest and publishing. It changes no state.
func publishCheck(db *sql.DB, logger *slog.Logger, auth *OwnerAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := ValidateID("id", id, ULID)
		if err == nil {
			var input struct{}
			err = DecodeJSON(r, &input)
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		runner, _ := r.Context().Value(runnerIdentityKey{}).(string)
		state, err := store.GetPublishCheckState(r.Context(), db, id)
		if errors.Is(err, sql.ErrNoRows) {
			WriteError(w, logger, &Error{Code: NotFound, Message: "Execution not found"})
			return
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		if runner == "" || state.RunnerID != runner {
			auth.reject(w, r, Forbidden, "execution_runner_mismatch", "Runner cannot check this Execution")
			return
		}
		if !state.Latest || state.Status != model.ExecutionPublishing {
			WriteError(w, logger, &Error{Code: Conflict, Message: "Publish check requires the latest publishing Execution"})
			return
		}
		writeJSON(w, http.StatusOK, publishCheckResponse{CancelRequested: state.CancelRequested,
			ApprovalPolicy: publishCheckPolicy{Version: 1, MergePullRequest: state.MergePullRequest}})
	}
}
