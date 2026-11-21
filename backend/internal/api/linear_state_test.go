package api

import (
	"net/http"
	"strings"
	"testing"
)

func requireLinearStateRow(t *testing.T, f *authFixture, taskID, executionID, kind, target, name, status string) {
	t.Helper()
	key := taskID + ":state_" + kind + ":" + target
	if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND execution_id=?
		AND action='set_state' AND kind=? AND dedupe_key=? AND state_name=? AND status=?
		AND body IS NULL AND label_name IS NULL AND outcome_unknown=0`, taskID, executionID, kind, key, name, status); n != 1 {
		t.Fatalf("state row %s/%s/%s = %d; want 1", key, name, status, n)
	}
}

// Real reports, answers and claims exercise repeated waits on the same Execution.
// The start after a decision uses its ID, including publish resumes that never
// enter Execution working. Duplicate report delivery must not supersede it.
func TestLinearStateDecisionAndResume(t *testing.T) {
	for _, source := range []string{"linear", "manual"} {
		for _, kind := range []string{"question", "approval", "blocked", "publish_unknown"} {
			t.Run(source+"/"+kind, func(t *testing.T) {
				f, token, id, taskID := reportFixture(t)
				if source == "linear" {
					linearTask(t, f, taskID)
				}
				approvalDeclarations(t, f, id)
				stage := map[string]string{"question": "work", "approval": "check", "blocked": "check", "publish_unknown": "publish"}[kind]
				attempt := runTo(t, f, token, id, stage)
				if source == "linear" {
					requireLinearStateRow(t, f, taskID, id, "started", id, "In Progress", "pending")
				}
				for round := range 2 {
					var input map[string]any
					decisionKind, answerBody := kind, `{"action":"continue"}`
					switch kind {
					case "question":
						input = finished(stage, attempt, nil)
						input["work"] = map[string]any{"sessionId": "session", "summaryArtifactId": nil,
							"question": map[string]any{"question": "Continue?"}}
						answerBody = `{"action":"answer","text":"yes","option":null}`
					case "approval":
						input = finished(stage, attempt, programResult(stage, "needs_approval", map[string]any{
							"contentId": "content-1", "approvalRequests": []any{stageApprovalRequest("merge_pull_request")}}))
						answerBody = `{"action":"approve","contentId":"content-1"}`
					case "blocked", "publish_unknown":
						decisionKind = "blocked"
						if stage == "check" {
							input = finished(stage, attempt, programResult(stage, "blocked", nil))
							answerBody = `{"action":"continue","contentId":"content-1"}`
						} else {
							input = finished(stage, attempt, programResult(stage, "unknown", map[string]any{"errorClass": "publish_unknown"}))
						}
					}
					for range 2 {
						finishStage(t, f, token, id, input)
					}
					decisionID := openDecisionID(t, f, id, decisionKind)
					if source == "linear" {
						requireLinearStateRow(t, f, taskID, id, "waiting_decision", decisionID, "In Review", "pending")
					}
					answer(t, f, decisionID, answerBody, 200)
					// Answering only queues the Execution; In Progress waits for claim.
					if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE kind='started' AND action='set_state'`); source == "linear" && n != round+1 {
						t.Fatalf("start written before claim: %d", n)
					}
					status, body := requestClaim(t, f, token)
					job := requireClaim(t, status, body)
					if job.Execution.ID != id {
						t.Fatal("resume created another Execution")
					}
					stage = "publish"
					if kind == "question" {
						stage = "work"
					}
					attempt = startStage(t, f, token, id, stage)
					// Replaying the old report after resumption must keep the new start.
					finishStage(t, f, token, id, input)
					if source == "linear" {
						requireLinearStateRow(t, f, taskID, id, "started", decisionID, "In Progress", "pending")
						requireLinearStateRow(t, f, taskID, id, "waiting_decision", decisionID, "In Review", "skipped")
						if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE action='set_state'`); n != 1+2*(round+1) {
							t.Fatalf("state rows=%d", n)
						}
						if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE action='set_state' AND status='pending'`); n != 1 {
							t.Fatalf("pending state rows=%d", n)
						}
						if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE action='comment' AND kind='decision_open' AND status='pending'`); n != round+1 {
							t.Fatalf("decision comments changed: %d", n)
						}
					} else if n := count(t, f, `SELECT count(*) FROM linear_outbox`); n != 0 {
						t.Fatalf("manual task wrote %d outbox rows", n)
					}
				}
			})
		}
	}
}

func TestLinearStateWaitFailureRollsBackDecisionAndTransition(t *testing.T) {
	f, token, id, taskID := reportFixture(t)
	linearTask(t, f, taskID)
	attempt := startStage(t, f, token, id, "work")
	execSQL(t, f, `CREATE TRIGGER reject_decision_state BEFORE INSERT ON linear_outbox
		WHEN NEW.kind='waiting_decision' BEGIN SELECT RAISE(ABORT,'state unavailable'); END`)
	input := finished("work", attempt, nil)
	input["work"] = map[string]any{"sessionId": "s", "summaryArtifactId": nil,
		"question": map[string]any{"question": "Continue?"}}
	status, body := postReport(t, f, token, id, input)
	if status != http.StatusInternalServerError {
		t.Fatalf("outbox failure=%d %s", status, body)
	}
	requireState(t, f, id, "working", "running", "")
	for _, table := range []string{"owner_decisions", "linear_outbox WHERE kind='decision_open'", "execution_events WHERE type='decision_opened'"} {
		if n := count(t, f, "SELECT count(*) FROM "+table); n != 0 {
			t.Fatalf("%s escaped rollback: %d", table, n)
		}
	}
	requireLinearStateRow(t, f, taskID, id, "started", id, "In Progress", "pending")
	execSQL(t, f, `DROP TRIGGER reject_decision_state`)
	finishStage(t, f, token, id, input)
	requireLinearStateRow(t, f, taskID, id, "waiting_decision", openDecisionID(t, f, id, "question"), "In Review", "pending")
}

func TestLinearStateOwnerAbort(t *testing.T) {
	for _, cause := range []string{"question", "check_blocked", "context_conflict", "workspace_missing", "publish_unknown"} {
		t.Run(cause, func(t *testing.T) {
			var f *authFixture
			var id string
			kind, input := "blocked", `{"action":"abort"}`
			switch cause {
			case "question":
				f, id = questionFixture(t)
				kind = "question"
			case "publish_unknown":
				f, id = publishUnknownFixture(t)
			default:
				f, id = checkBlockedFixture(t)
				if cause == "check_blocked" {
					input = `{"action":"abort","contentId":"content-1"}`
				} else {
					execSQL(t, f, `UPDATE owner_decisions SET cause=?,basis=NULL WHERE execution_id=? AND status='open'`, cause, id)
				}
			}
			var taskID string
			if err := f.db.QueryRow(`SELECT task_id FROM executions WHERE id=?`, id).Scan(&taskID); err != nil {
				t.Fatal(err)
			}
			linearTask(t, f, taskID)
			answer(t, f, openDecisionID(t, f, id, kind), input, 200)
			if cause == "publish_unknown" {
				if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE action='set_state'`); n != 0 {
					t.Fatal("failed Execution wrote state")
				}
			} else {
				requireLinearStateRow(t, f, taskID, id, "cancelled", id, "Canceled", "pending")
			}
		})
	}
}

func TestLinearStateTwoApprovalsResumeAfterLastAnswer(t *testing.T) {
	f, token, id, taskID := reportFixture(t)
	linearTask(t, f, taskID)
	approvalDeclarations(t, f, id)
	attempt := runTo(t, f, token, id, "check")
	finishStage(t, f, token, id, finished("check", attempt, programResult("check", "needs_approval", map[string]any{
		"approvalRequests": []any{stageApprovalRequest("merge_pull_request"), stageApprovalRequest("deploy")},
	})))
	first := openDecisionID(t, f, id, "approval")
	answer(t, f, first, `{"action":"approve","contentId":"content-1"}`, 200)
	if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE kind='waiting_decision' AND action='set_state'`); n != 2 {
		t.Fatalf("waiting rows=%d; want one per decision", n)
	}
	status, body := requestClaim(t, f, token)
	if status != 200 || !strings.Contains(string(body), `"job":null`) {
		t.Fatalf("claim with another approval pending=%d %s", status, body)
	}
	last := openDecisionID(t, f, id, "approval")
	answer(t, f, last, `{"action":"approve","contentId":"content-1"}`, 200)
	status, body = requestClaim(t, f, token)
	job := requireClaim(t, status, body)
	if job.Execution.ID != id {
		t.Fatalf("resumed Execution=%s", job.Execution.ID)
	}
	requireLinearStateRow(t, f, taskID, id, "started", last, "In Progress", "pending")
	if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE action='set_state' AND kind='started'`); n != 2 {
		t.Fatalf("start rows=%d; want initial start and one resume", n)
	}
}

func TestLinearStateResumeFailureRollsBackClaim(t *testing.T) {
	f, token, id, taskID := reportFixture(t)
	linearTask(t, f, taskID)
	attempt := startStage(t, f, token, id, "work")
	input := finished("work", attempt, nil)
	input["work"] = map[string]any{"sessionId": "s", "summaryArtifactId": nil,
		"question": map[string]any{"question": "Continue?"}}
	finishStage(t, f, token, id, input)
	decision := openDecisionID(t, f, id, "question")
	answer(t, f, decision, `{"action":"answer","text":"yes","option":null}`, 200)
	execSQL(t, f, `CREATE TRIGGER reject_resume_state BEFORE INSERT ON linear_outbox
		WHEN NEW.kind='started' BEGIN SELECT RAISE(ABORT,'state unavailable'); END`)
	status, body := requestClaim(t, f, token)
	if status != http.StatusInternalServerError {
		t.Fatalf("outbox failure=%d %s", status, body)
	}
	requireResume(t, f, id, "work")
	requireLinearStateRow(t, f, taskID, id, "waiting_decision", decision, "In Review", "pending")
	execSQL(t, f, `DROP TRIGGER reject_resume_state`)
	status, body = requestClaim(t, f, token)
	job := requireClaim(t, status, body)
	if job.Execution.ID != id {
		t.Fatalf("resumed Execution=%s", job.Execution.ID)
	}
	requireLinearStateRow(t, f, taskID, id, "started", decision, "In Progress", "pending")
}
