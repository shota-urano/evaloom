package api

import (
	"context"
	"database/sql"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
)

func resumeOutboxFixture(t *testing.T, source, cause string) (*authFixture, string, string, resumeInput) {
	t.Helper()
	f, _, id, input := resumeFixture(t, cause)
	taskID := executionTaskID(t, f, id)
	if source == "manual" {
		return f, taskID, id, input
	}
	if _, err := f.db.Exec(`UPDATE tasks SET source = 'linear' WHERE id = ?`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url,
		team_id, task_id, attach_seq, created_at)
		VALUES ('resume-link', 'resume-issue', 'ENG-1', 'https://linear.app/x/ENG-1', 'team-a', ?, 1, 't')`, taskID); err != nil {
		t.Fatal(err)
	}
	// Preserve the pending start and Blocked comment that precede the decision.
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	trigger := linearoutbox.Trigger{TaskID: taskID, ExecutionID: id, TargetID: id, Kind: linearoutbox.KindStarted}
	if err := linearoutbox.EnqueueStateTx(context.Background(), tx, trigger); err != nil {
		t.Fatal(err)
	}
	trigger.Kind = linearoutbox.KindBlocked
	if err := linearoutbox.EnqueueTx(context.Background(), tx, trigger); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f, taskID, id, input
}

func resumeOutboxSnapshot(t *testing.T, f *authFixture) string {
	t.Helper()
	var snapshot string
	if err := f.db.QueryRow(`SELECT json_group_array(json_object('id', id, 'action', action,
		'kind', kind, 'state', state_name, 'status', status, 'key', dedupe_key))
		FROM (SELECT * FROM linear_outbox ORDER BY id)`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestResumeLinearStateForOwnerDecision(t *testing.T) {
	for _, source := range []string{"linear", "manual"} {
		for _, decision := range []string{"abort", "continue"} {
			for _, cause := range resumeCauses {
				if decision == "continue" && (cause == "context_conflict" || cause == "git_state") {
					continue // These causes only allow abort (spec 04 3.8).
				}
				t.Run(source+"/"+decision+"/"+cause, func(t *testing.T) {
					f, taskID, id, input := resumeOutboxFixture(t, source, cause)
					before := resumeOutboxSnapshot(t, f)
					input.Decision = decision
					status, body := requestResume(t, f, taskID, input)
					requireResumeSuccess(t, status, body)
					requireSavedDecision(t, f, input)
					if source == "linear" && decision == "abort" {
						var issueID, kind, name, stateStatus, executionID, key string
						var stateBody, commentID sql.NullString
						var unknown, attempts int
						if err := f.db.QueryRow(`SELECT linear_issue_id, kind, state_name, status, execution_id,
							dedupe_key, body, linear_comment_id, outcome_unknown, attempts FROM linear_outbox
							WHERE task_id = ? AND action = 'set_state' AND kind = 'cancelled'`, taskID).
							Scan(&issueID, &kind, &name, &stateStatus, &executionID, &key, &stateBody, &commentID, &unknown, &attempts); err != nil {
							t.Fatal(err)
						}
						if issueID != "resume-issue" || kind != "cancelled" || name != "Canceled" || stateStatus != "pending" ||
							executionID != id || key != taskID+":state_cancelled:"+id || stateBody.Valid || commentID.Valid || unknown != 0 || attempts != 0 {
							t.Fatalf("cancel state=%s/%s/%s/%s execution=%s key=%s body=%v comment=%v unknown=%d attempts=%d",
								issueID, kind, name, stateStatus, executionID, key, stateBody, commentID, unknown, attempts)
						}
						var total, skipped, comments int
						if err := f.db.QueryRow(`SELECT count(*),
							count(*) FILTER (WHERE action = 'set_state' AND kind = 'started' AND status = 'skipped'),
							count(*) FILTER (WHERE action = 'comment' AND status = 'pending')
							FROM linear_outbox WHERE task_id = ?`, taskID).Scan(&total, &skipped, &comments); err != nil {
							t.Fatal(err)
						}
						if total != 4 || skipped != 1 || comments != 2 || resumeState(t, f, id).Task != "cancelled" {
							t.Fatalf("outbox total=%d skipped=%d pending comments=%d", total, skipped, comments)
						}
					} else if after := resumeOutboxSnapshot(t, f); after != before {
						t.Fatalf("unexpected outbox change: before=%s after=%s", before, after)
					}
					// A repeated API request cannot create another state or comment.
					before = resumeOutboxSnapshot(t, f)
					status, body = requestResume(t, f, taskID, input)
					assertAuthError(t, status, body, 409, Conflict)
					if after := resumeOutboxSnapshot(t, f); after != before {
						t.Fatalf("repeated decision changed outbox: before=%s after=%s", before, after)
					}
				})
			}
		}
	}
}

func TestResumeLinearAbortRollsBackStateAndOutbox(t *testing.T) {
	for _, trigger := range []string{
		`BEFORE INSERT ON linear_outbox WHEN NEW.action = 'set_state'`,
		`BEFORE UPDATE OF status ON linear_outbox WHEN NEW.status = 'skipped'`,
		`BEFORE INSERT ON audit_logs WHEN NEW.action = 'task.resume'`,
	} {
		t.Run(trigger, func(t *testing.T) {
			f, taskID, id, input := resumeOutboxFixture(t, "linear", "gate2_missing_tests")
			input.Decision = "abort"
			before, outboxBefore := resumeState(t, f, id), resumeOutboxSnapshot(t, f)
			if _, err := f.db.Exec(`CREATE TRIGGER reject_resume_state ` + trigger + ` BEGIN SELECT RAISE(ABORT, 'state unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			status, body := requestResume(t, f, taskID, input)
			assertAuthError(t, status, body, 500, Internal)
			requireResumeUnchanged(t, f, id, before)
			if after := resumeOutboxSnapshot(t, f); after != outboxBefore {
				t.Fatalf("outbox survived rollback: before=%s after=%s", outboxBefore, after)
			}
		})
	}
}
