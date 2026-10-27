package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

func claimExecution(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// protectRunner has already matched this identity to the route's Runner.
		runner, _ := r.Context().Value(runnerIdentityKey{}).(string)
		if runner == "" {
			WriteError(w, logger, &Error{Code: Forbidden, Message: "Runner identity required"})
			return
		}
		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil) // store.Open configures BEGIN IMMEDIATE.
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		claim, err := store.NextClaim(ctx, tx, runner)
		if errors.Is(err, store.ErrClaimAssignmentMissing) {
			err = &Error{Code: PreconditionFailed, Message: err.Error(),
				Details: []Detail{{Field: "projectId", Reason: "default_harness_missing"}}}
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		if claim == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		err = task.TransitionExecutionTx(ctx, tx, task.Transition{
			TaskID: claim.Execution.TaskID, ExecutionID: claim.Execution.ID,
			FromTask: model.TaskQueued, FromExecution: model.ExecutionPending, ToExecution: model.ExecutionClaimed,
		})
		if errors.Is(err, task.ErrConflict) {
			err = &Error{Code: Conflict, Message: err.Error()}
		}
		if err == nil {
			err = store.PinClaim(ctx, tx, claim, runner, time.Now().UTC().Format(time.RFC3339))
		}
		if err == nil {
			_, err = store.AppendExecutionEvent(ctx, tx, claim.Execution.ID, model.EventStatusChanged,
				json.RawMessage(`{"from":"pending","to":"claimed","reason":"runner_claimed"}`))
		}
		if err == nil {
			_, err = store.RecordAuditLog(ctx, tx, store.AuditLog{
				Actor: "runner:" + runner, Action: "execution.claim", TargetType: "execution", TargetID: claim.Execution.ID,
				Detail: map[string]any{"taskId": claim.Execution.TaskID, "projectId": claim.Project.ID,
					"from": model.ExecutionPending, "to": model.ExecutionClaimed,
					"profileId": claim.Assignment.ProfileID, "profileCommitSha": claim.Profile.CommitSHA},
			})
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, claim)
	}
}
