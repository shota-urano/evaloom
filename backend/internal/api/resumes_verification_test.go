package api

import (
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func gate2BlockedVerificationFixture(t *testing.T, cause string) (*authFixture, string, resumeInput) {
	t.Helper()
	f, token, id, input := verifyResultFixture(t)
	delete(input, "verifiedSha")
	input["status"] = "blocked"
	input["gateResults"] = []any{gateReport(2, "blocked")}
	input["block"] = map[string]any{"cause": cause, "headSha": strings.Repeat("c", 40)}
	if status, body := postResult(t, f, token, id, input); status != 204 {
		t.Fatalf("block=%d %s", status, body)
	}
	decision := resumeInput{Decision: "continue"}
	if err := f.db.QueryRow(`SELECT id,head_sha FROM execution_blocks WHERE execution_id=?`, id).Scan(&decision.BlockID, &decision.HeadSHA); err != nil {
		t.Fatal(err)
	}
	return f, id, decision
}

func TestResumeGate2BindsOnlyOriginalSuccessfulMeasurement(t *testing.T) {
	for _, name := range []string{"MissingTests", "Unverifiable", "MissingRun", "UnknownCount", "AllSkipped", "WrongSHA", "ForeignExecution", "NonzeroExit", "FailedTests", "TimedOut", "Abort"} {
		t.Run(name, func(t *testing.T) {
			cause := "gate2_missing_tests"
			if name == "Unverifiable" {
				cause = "gate2_unverifiable"
			}
			f, id, input := gate2BlockedVerificationFixture(t, cause)
			// Simulate missing or inconsistent stored evidence. None of these
			// records may be converted into verification by owner approval.
			var mutation string
			switch name {
			case "MissingRun":
				mutation = `DELETE FROM test_runs WHERE kind='verify'`
			case "UnknownCount":
				mutation = `UPDATE test_runs SET total=NULL WHERE kind='verify'`
			case "AllSkipped":
				mutation = `UPDATE test_runs SET skipped=total,passed=0 WHERE kind='verify'`
			case "WrongSHA":
				mutation = `UPDATE test_runs SET target_sha='` + strings.Repeat("d", 40) + `' WHERE kind='verify'`
			case "ForeignExecution":
				mutation = `UPDATE test_runs SET kind='baseline' WHERE kind='verify'`
			case "NonzeroExit":
				mutation = `UPDATE test_runs SET exit_code=1 WHERE kind='verify'`
			case "FailedTests":
				mutation = `UPDATE test_runs SET failed=1,passed=1 WHERE kind='verify'`
			case "TimedOut":
				mutation = `UPDATE test_runs SET timed_out=1 WHERE kind='verify'`
			case "Abort":
				input.Decision = "abort"
			}
			if mutation != "" {
				if _, err := f.db.Exec(mutation); err != nil {
					t.Fatal(err)
				}
			}
			if name == "ForeignExecution" {
				// A valid measurement on another Execution must not be reused.
				other := createClaimTask(t, f, "project-a", "other")
				if _, err := f.db.Exec(`UPDATE test_runs SET execution_id=(SELECT id FROM executions WHERE task_id=?),kind='verify'`, other.ID); err != nil {
					t.Fatal(err)
				}
			}
			status, body := requestResume(t, f, executionTaskID(t, f, id), input)
			requireResumeSuccess(t, status, body)
			var verified string
			if err := f.db.QueryRow(`SELECT coalesce(verified_sha,'') FROM executions WHERE id=?`, id).Scan(&verified); err != nil {
				t.Fatal(err)
			}
			want := ""
			if name == "MissingTests" || name == "Unverifiable" {
				want = input.HeadSHA
			}
			if verified != want {
				t.Fatalf("verified=%q want=%q", verified, want)
			}
		})
	}
}

func TestResumeGate2VerificationRollsBackWithDecision(t *testing.T) {
	f, id, input := gate2BlockedVerificationFixture(t, string(model.BlockGate2MissingTests))
	before := resumeState(t, f, id)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_approval BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT,'fixture approval write failure'); END`); err != nil {
		t.Fatal(err)
	}
	status, body := requestResume(t, f, executionTaskID(t, f, id), input)
	assertAuthError(t, status, body, 500, Internal)
	requireResumeUnchanged(t, f, id, before)
}
