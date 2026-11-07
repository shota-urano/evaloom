package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

func claimExecution(db *sql.DB, logger *slog.Logger, artifacts ArtifactConfig) http.HandlerFunc {
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
		// ArtifactStore.Open uses the DB itself. Read after the claim transaction
		// has committed, so it does not contend with that transaction. Failure to
		// read IDs must still deliver the claim with null IDs (Q40).
		if claim.Worktree != nil && claim.Worktree.Baseline != nil {
			baseline := claim.Worktree.Baseline
			baseline.TestIDs = claimBaselineIDs(ctx, artifacts, baseline.TestRun)
		}
		claim.Execution.ResumeHarnessSummary = claimHarnessSummary(ctx, artifacts, claim.Execution.ID, claim.HarnessOutputArtifactID)
		writeJSON(w, http.StatusOK, claim)
	}
}

func claimHarnessSummary(ctx context.Context, config ArtifactConfig, executionID string, artifactID *string) *string {
	if artifactID == nil || config.Store == nil {
		return nil
	}
	artifact, file, err := config.Store.Open(ctx, executionID, *artifactID)
	if err != nil {
		return nil
	}
	defer file.Close()
	if artifact.Kind != "harness_output" {
		return nil
	}
	body, err := io.ReadAll(file)
	if err != nil || !utf8.Valid(body) {
		return nil
	}
	var result struct {
		Summary *string `json:"summary"`
	}
	if json.Unmarshal(body, &result) != nil {
		return nil
	}
	return result.Summary
}

func claimBaselineIDs(ctx context.Context, config ArtifactConfig, run model.TestRun) []string {
	if run.TestIDsArtifactID == nil || config.Store == nil {
		return nil
	}
	artifact, file, err := config.Store.Open(ctx, run.ExecutionID, *run.TestIDsArtifactID)
	if err != nil {
		return nil
	}
	defer file.Close()
	if artifact.Kind != "test_ids" {
		return nil
	}
	body, err := io.ReadAll(file)
	if err != nil || !utf8.Valid(body) {
		return nil
	}
	// []string alone also accepts null elements. Require every JSON element
	// to be a string, preserving its exact order and multiplicity.
	var values []any
	if json.Unmarshal(body, &values) != nil || values == nil {
		return nil
	}
	if run.Total == nil || run.Skipped == nil || *run.Skipped < 0 || *run.Total < *run.Skipped || len(values) != *run.Total-*run.Skipped {
		return nil
	}
	ids := make([]string, len(values))
	for i, value := range values {
		id, ok := value.(string)
		if !ok {
			return nil
		}
		ids[i] = id
	}
	return ids
}
