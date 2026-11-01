package api

import (
	"database/sql"
	"net/http"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Gate 2 measures once. Its record, gate result, SHAs, Block and transition
// commit together through the existing result endpoint, without a new route.
func validateVerifyRuns(r *http.Request, tx *sql.Tx, execution store.ContextExecution, input resultInput) error {
	if len(input.TestRuns) == 0 {
		return nil
	}
	if len(input.TestRuns) != 1 || input.FromStatus != "verifying" {
		return invalid("testRuns", "single_gate2_run_required")
	}
	run := input.TestRuns[0]
	if err := ValidateID("testRuns.id", run.ID, ULID); err != nil {
		return err
	}
	if run.Kind != "verify" || run.ExecutionID != execution.ID || !fullCommitSHA(run.TargetSHA) {
		return invalid("testRuns", "execution_kind_or_sha_mismatch")
	}
	if run.StartedAt.IsZero() || run.FinishedAt.IsZero() || run.FinishedAt.Before(run.StartedAt) {
		return invalid("testRuns", "invalid_times")
	}
	for _, count := range []*int{run.Total, run.Passed, run.Failed, run.Skipped} {
		if count != nil && *count < 0 {
			return invalid("testRuns", "negative_count")
		}
	}
	if run.Total != nil && run.Passed != nil && run.Failed != nil && run.Skipped != nil {
		remaining := *run.Total
		for _, count := range []int{*run.Passed, *run.Failed, *run.Skipped} {
			if count > remaining {
				return invalid("testRuns", "inconsistent_counts")
			}
			remaining -= count
		}
		if remaining != 0 {
			return invalid("testRuns", "inconsistent_counts")
		}
	}
	var command sql.NullString
	var head, verified string
	if err := tx.QueryRowContext(r.Context(), `SELECT p.test_command, coalesce(?,e.harness_head_sha,''), coalesce(?,e.verified_sha,'')
		FROM executions e JOIN tasks t ON t.id=e.task_id JOIN projects p ON p.id=t.project_id WHERE e.id=?`,
		input.HarnessHeadSHA, input.VerifiedSHA, execution.ID).Scan(&command, &head, &verified); err != nil {
		return err
	}
	if !command.Valid || command.String != run.Command || run.Command == "" {
		return invalid("testRuns.command", "registry_mismatch")
	}
	if head != run.TargetSHA {
		return &Error{Code: PreconditionFailed, Message: "Verify run must test the harness head SHA"}
	}
	if input.VerifiedSHA != nil && verified != run.TargetSHA {
		return invalid("verifiedSha", "verify_run_mismatch")
	}
	if run.TimedOut && input.Status != "timed_out" {
		return invalid("testRuns.timedOut", "status_mismatch")
	}
	if run.TestIDsArtifactID != nil {
		if err := ValidateID("testRuns.testIdsArtifactId", *run.TestIDsArtifactID, ULID); err != nil {
			return err
		}
		var own bool
		if err := tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM artifacts WHERE id=? AND execution_id=? AND kind='test_ids')`, *run.TestIDsArtifactID, execution.ID).Scan(&own); err != nil {
			return err
		}
		if !own {
			return &Error{Code: PreconditionFailed, Message: "Verify IDs must belong to this Execution"}
		}
	}
	for _, result := range input.GateResults {
		if result.Gate == 2 && result.Result == "pass" && (run.ExitCode == nil || *run.ExitCode != 0 || run.Total == nil || run.Skipped == nil || run.Failed == nil || *run.Failed > 0 || run.TimedOut || input.VerifiedSHA == nil) {
			return invalid("testRuns", "gate_pass_requires_successful_measurement")
		}
	}
	return nil
}
