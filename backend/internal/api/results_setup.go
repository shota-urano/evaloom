package api

import (
	"database/sql"
	"errors"
	"net/http"
	"reflect"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func hasSetupResult(input resultInput) bool {
	if input.SetupStatus != nil {
		return true
	}
	for _, run := range input.TestRuns {
		if run.Kind == "setup" {
			return true
		}
	}
	return false
}

// Like baseline, setup is acknowledged without changing preparing. A terminal
// failure is reported afterwards. Only identical measurements may be retried
// after that transition; SaveSetupTx rejects new records outside preparing.
func saveSetupResult(r *http.Request, tx *sql.Tx, execution store.ContextExecution, input resultInput) error {
	if !execution.Latest || input.FromStatus != "preparing" || input.Status != "preparing" {
		return &Error{Code: Conflict, Message: "Setup must be reported during preparation of the latest Execution"}
	}
	expected := resultInput{FromStatus: input.FromStatus, Status: input.Status,
		WorktreeID: input.WorktreeID, TestRuns: input.TestRuns, SetupStatus: input.SetupStatus}
	if !reflect.DeepEqual(input, expected) {
		return invalid("testRuns", "setup_measurement_only")
	}
	if input.WorktreeID == nil || input.SetupStatus == nil || len(input.TestRuns) != 1 {
		return invalid("testRuns", "single_setup_run_and_worktree_status_required")
	}
	if err := ValidateID("worktreeId", *input.WorktreeID, ULID); err != nil {
		return err
	}
	if *input.SetupStatus != "succeeded" && *input.SetupStatus != "failed" {
		return invalid("setupStatus", "invalid_value")
	}
	run := input.TestRuns[0]
	if run.ExecutionID != execution.ID || run.Kind != "setup" || !fullCommitSHA(run.TargetSHA) {
		return invalid("testRuns", "execution_kind_or_sha_mismatch")
	}
	err := (store.SetupStore{}).SaveSetupTx(r.Context(), tx, *input.WorktreeID, run, *input.SetupStatus == "succeeded")
	switch {
	case errors.Is(err, store.ErrInvalidSetup):
		return invalid("testRuns", "invalid_setup_measurement")
	case errors.Is(err, store.ErrBaselineScope):
		return &Error{Code: PreconditionFailed, Message: "Setup does not match the prepared worktree and start SHA"}
	case errors.Is(err, store.ErrSetupExists):
		return &Error{Code: Conflict, Message: "Setup measurement is immutable"}
	default:
		return err
	}
}
