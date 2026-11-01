package api

import (
	"database/sql"
	"errors"
	"net/http"
	"reflect"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func saveBaselineResult(r *http.Request, tx *sql.Tx, execution store.ContextExecution, input resultInput) error {
	if input.FromStatus != "preparing" || input.Status != "preparing" {
		return &Error{Code: Conflict, Message: "Baseline must be reported during preparation"}
	}
	// An idempotent measurement acknowledgement must never replay a transition,
	// a Gate result, or other side effects bundled into the same request.
	expected := resultInput{FromStatus: input.FromStatus, Status: input.Status,
		WorktreeID: input.WorktreeID, BaseSHA: input.BaseSHA, Baseline: input.Baseline}
	if !reflect.DeepEqual(input, expected) {
		return invalid("baseline", "measurement_only")
	}
	if input.WorktreeID == nil || input.BaseSHA == nil {
		return invalid("baseline", "worktree_and_start_sha_required")
	}
	var savedWorktree sql.NullString
	if err := tx.QueryRowContext(r.Context(), `SELECT worktree_id FROM executions WHERE id = ?`, execution.ID).Scan(&savedWorktree); err != nil {
		return err
	}
	if savedWorktree.Valid && savedWorktree.String != *input.WorktreeID {
		return &Error{Code: PreconditionFailed, Message: "Baseline worktree must equal the recorded worktree"}
	}
	if err := validateResult(r, tx, execution, input); err != nil {
		return err
	}
	run := input.Baseline
	if err := ValidateID("baseline.id", run.ID, ULID); err != nil {
		return err
	}
	if run.ExecutionID != execution.ID || run.Kind != "baseline" || run.TargetSHA != *input.BaseSHA {
		return invalid("baseline", "execution_or_start_sha_mismatch")
	}
	if run.StartedAt.IsZero() || run.FinishedAt.IsZero() || run.FinishedAt.Before(run.StartedAt) {
		return invalid("baseline", "invalid_times")
	}
	for _, count := range []*int{run.Total, run.Passed, run.Failed, run.Skipped} {
		if count != nil && *count < 0 {
			return invalid("baseline", "negative_count")
		}
	}
	if run.Total != nil && run.Passed != nil && run.Failed != nil && run.Skipped != nil {
		remaining := *run.Total
		for _, count := range []int{*run.Passed, *run.Failed, *run.Skipped} {
			if count > remaining {
				return invalid("baseline", "inconsistent_counts")
			}
			remaining -= count
		}
		if remaining != 0 {
			return invalid("baseline", "inconsistent_counts")
		}
	}
	var command sql.NullString
	if err := tx.QueryRowContext(r.Context(), `SELECT p.test_command FROM projects p JOIN tasks t ON t.project_id = p.id WHERE t.id = ?`, execution.TaskID).Scan(&command); err != nil {
		return err
	}
	if !command.Valid || command.String == "" || command.String != run.Command {
		return invalid("baseline.command", "registry_mismatch")
	}
	if run.TestIDsArtifactID != nil {
		if err := ValidateID("baseline.testIdsArtifactId", *run.TestIDsArtifactID, ULID); err != nil {
			return err
		}
		var own bool
		if err := tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM artifacts WHERE id = ? AND execution_id = ? AND kind = 'test_ids')`, *run.TestIDsArtifactID, execution.ID).Scan(&own); err != nil {
			return err
		}
		if !own {
			return &Error{Code: PreconditionFailed, Message: "Baseline IDs must belong to this Execution"}
		}
	}
	// Associate the prepared worktree and immutable start SHA before checking
	// BaselineStore's scope, within this same transaction.
	if err := saveResult(r, tx, execution, input); err != nil {
		return err
	}
	err := (store.BaselineStore{}).SaveBaselineTx(r.Context(), tx, *input.WorktreeID, *run)
	if errors.Is(err, store.ErrBaselineScope) {
		return &Error{Code: PreconditionFailed, Message: "Baseline does not match the prepared worktree"}
	}
	if errors.Is(err, store.ErrBaselineExists) {
		return &Error{Code: Conflict, Message: "Worktree baseline is immutable"}
	}
	return err
}
