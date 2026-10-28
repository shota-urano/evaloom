package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"
)

var resumeCauses = []string{"gate1_test_change", "gate1_unverifiable", "gate2_missing_tests", "gate2_unverifiable", "gate3_mismatch", "context_conflict", "git_state"}

func resumeFixture(t *testing.T, cause string) (*authFixture, string, string, resumeInput) {
	t.Helper()
	f, token, id, _ := resultFixture(t, "verifying")
	input := resultBody("verifying", "blocked")
	input["block"].(map[string]any)["cause"] = cause
	status, body := postResult(t, f, token, id, input)
	if status != 204 {
		t.Fatalf("block fixture=%d %s", status, body)
	}
	decision := resumeInput{Decision: "continue"}
	if err := f.db.QueryRow(`SELECT id, head_sha FROM execution_blocks WHERE execution_id = ?`, id).
		Scan(&decision.BlockID, &decision.HeadSHA); err != nil {
		t.Fatal(err)
	}
	return f, token, id, decision
}

func requestResumeJSON(t *testing.T, f *authFixture, taskID, body string, alter func(*http.Request)) (int, []byte) {
	t.Helper()
	return f.request(t, "POST", "/api/tasks/"+taskID+"/resume", true, f.csrf, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		r.Body = io.NopCloser(strings.NewReader(body))
		if alter != nil {
			alter(r)
		}
	})
}

func requestResume(t *testing.T, f *authFixture, taskID string, input resumeInput) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return requestResumeJSON(t, f, taskID, string(body), nil)
}

type resumeSnapshot struct {
	Execution                        []any
	Task, Updated, Worktree, Blocks  string
	Events, Audits, Notifications, N int
}

// Preserve every Execution column and all Block evidence when a request fails.
func resumeState(t *testing.T, f *authFixture, id string) resumeSnapshot {
	t.Helper()
	s := resumeSnapshot{Execution: retryExecutionSnapshot(t, f.db, id)}
	err := f.db.QueryRow(`SELECT t.status, t.updated_at, w.status,
		(SELECT json_group_array(json_object('id', id, 'cause', cause, 'head', head_sha,
		'detail', detail, 'gate', gate_result_id, 'decision', decision, 'decidedAt', decided_at, 'createdAt', created_at))
		FROM execution_blocks WHERE execution_id = e.id),
		(SELECT count(*) FROM execution_events WHERE execution_id = e.id),
		(SELECT count(*) FROM audit_logs WHERE action = 'task.resume' AND target_id = t.id),
		(SELECT count(*) FROM notification_requests WHERE execution_id = e.id),
		(SELECT count(*) FROM executions WHERE task_id = t.id)
		FROM executions e JOIN tasks t ON t.id = e.task_id JOIN worktrees w ON w.id = e.worktree_id
		WHERE e.id = ?`, id).Scan(&s.Task, &s.Updated, &s.Worktree, &s.Blocks, &s.Events, &s.Audits, &s.Notifications, &s.N)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func requireResumeUnchanged(t *testing.T, f *authFixture, id string, before resumeSnapshot) {
	t.Helper()
	if after := resumeState(t, f, id); !reflect.DeepEqual(after, before) {
		t.Fatalf("request changed DB: before=%+v after=%+v", before, after)
	}
}

func requireResumeSuccess(t *testing.T, status int, body []byte) {
	t.Helper()
	if status != 204 || len(body) != 0 {
		t.Fatalf("resume=%d %s", status, body)
	}
}

func requireSavedDecision(t *testing.T, f *authFixture, input resumeInput) {
	t.Helper()
	var decision, decidedAt string
	if err := f.db.QueryRow(`SELECT decision, decided_at FROM execution_blocks WHERE id = ?`, input.BlockID).
		Scan(&decision, &decidedAt); err != nil || decision != input.Decision {
		t.Fatalf("decision=%s at=%s err=%v", decision, decidedAt, err)
	}
	if _, err := ParseTimestamp(decidedAt); err != nil {
		t.Fatal(err)
	}
}

func TestResumeHeadSHAMismatchReturns422WithoutWrites(t *testing.T) {
	for _, decision := range []string{"continue", "abort"} {
		t.Run(decision, func(t *testing.T) {
			f, _, id, input := resumeFixture(t, "gate1_test_change")
			input.Decision, input.HeadSHA = decision, strings.Repeat("c", 40)
			before := resumeState(t, f, id)
			status, body := requestResume(t, f, executionTaskID(t, f, id), input)
			assertAuthError(t, status, body, 422, PreconditionFailed)
			requireResumeUnchanged(t, f, id, before)
		})
	}
}

func TestResumeContextConflictAndGitStateContinueReturn422(t *testing.T) {
	for _, cause := range []string{"context_conflict", "git_state"} {
		t.Run(cause, func(t *testing.T) {
			f, _, id, input := resumeFixture(t, cause)
			before := resumeState(t, f, id)
			status, body := requestResume(t, f, executionTaskID(t, f, id), input)
			assertAuthError(t, status, body, 422, PreconditionFailed)
			requireResumeUnchanged(t, f, id, before)
		})
	}
}

func TestResumeContinueReturnsSameExecutionToPendingWithResumePoint(t *testing.T) {
	for _, tc := range []struct{ cause, point string }{
		{"gate1_test_change", "gate2"}, {"gate1_unverifiable", "gate2"},
		{"gate2_missing_tests", "gate3"}, {"gate2_unverifiable", "gate3"}, {"gate3_mismatch", "gate3"},
	} {
		t.Run(tc.cause, func(t *testing.T) {
			f, token, id, input := resumeFixture(t, tc.cause)
			taskID := executionTaskID(t, f, id)
			var pin, worktreeID string
			if err := f.db.QueryRow(`SELECT profile_id, worktree_id FROM executions WHERE id = ?`, id).Scan(&pin, &worktreeID); err != nil {
				t.Fatal(err)
			}
			before := resumeState(t, f, id)
			status, body := requestResume(t, f, taskID, input)
			requireResumeSuccess(t, status, body)
			requireSavedDecision(t, f, input)
			var state, point, savedPin, savedWorktree string
			var finished sql.NullString
			if err := f.db.QueryRow(`SELECT status, resume_point, profile_id, worktree_id, finished_at
				FROM executions WHERE id = ?`, id).Scan(&state, &point, &savedPin, &savedWorktree, &finished); err != nil ||
				state != "pending" || point != tc.point || savedPin != pin || savedWorktree != worktreeID || finished.Valid {
				t.Fatalf("pending=%s point=%s pin=%s worktree=%s finished=%v err=%v", state, point, savedPin, savedWorktree, finished, err)
			}
			after := resumeState(t, f, id)
			if after.Task != "queued" || after.N != before.N || after.Worktree != "retained" ||
				after.Events != before.Events+1 || after.Audits != 1 || after.Notifications != before.Notifications {
				t.Fatalf("continuation=%+v", after)
			}
			// Reclaim uses the saved Profile even when the assignment is gone.
			if _, err := f.db.Exec(`DELETE FROM project_harnesses WHERE project_id = 'project-a'`); err != nil {
				t.Fatal(err)
			}
			status, body = requestClaim(t, f, token)
			claim := requireClaim(t, status, body)
			if claim.Execution.ID != id || claim.Execution.ResumePoint == nil || *claim.Execution.ResumePoint != tc.point {
				t.Fatalf("reclaim=%+v", claim.Execution)
			}
			status, body = postResult(t, f, token, id, resultBody("claimed", "verifying"))
			requireResumeSuccess(t, status, body)
		})
	}
}

func TestResumeAbortCancelsTaskAndRetainsWorktree(t *testing.T) {
	for _, cause := range resumeCauses {
		t.Run(cause, func(t *testing.T) {
			f, token, id, input := resumeFixture(t, cause)
			input.Decision = "abort"
			path := filepath.Join(t.TempDir(), "work.txt")
			if err := os.WriteFile(path, []byte("retained work"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE worktrees SET path = ? WHERE id = (SELECT worktree_id FROM executions WHERE id = ?)`, filepath.Dir(path), id); err != nil {
				t.Fatal(err)
			}
			before := resumeState(t, f, id)
			status, body := requestResume(t, f, executionTaskID(t, f, id), input)
			requireResumeSuccess(t, status, body)
			requireSavedDecision(t, f, input)
			var state, classification string
			var finished, point sql.NullString
			if err := f.db.QueryRow(`SELECT status, error_class, finished_at, resume_point FROM executions WHERE id = ?`, id).
				Scan(&state, &classification, &finished, &point); err != nil || state != "cancelled" || classification != "cancelled" || !finished.Valid || point.Valid {
				t.Fatalf("abort state=%s class=%s finished=%v point=%v err=%v", state, classification, finished, point, err)
			}
			if _, err := ParseTimestamp(finished.String); err != nil {
				t.Fatal(err)
			}
			after := resumeState(t, f, id)
			if after.Task != "cancelled" || after.Worktree != "retained" || after.N != before.N ||
				after.Events != before.Events+1 || after.Audits != 1 || after.Notifications != before.Notifications {
				t.Fatalf("abort=%+v", after)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "retained work" {
				t.Fatalf("worktree content=%s err=%v", data, err)
			}
			status, body = requestClaim(t, f, token)
			requireResumeSuccess(t, status, body) // No cancelled work is offered.
		})
	}
}

func TestResumeDecisionIsRecordedInAuditLogs(t *testing.T) {
	for _, decision := range []string{"continue", "abort"} {
		t.Run(decision, func(t *testing.T) {
			f, _, id, input := resumeFixture(t, "gate1_test_change")
			input.Decision = decision
			taskID := executionTaskID(t, f, id)
			status, body := requestResume(t, f, taskID, input)
			requireResumeSuccess(t, status, body)
			var actor, targetType, targetID, detail, createdAt string
			if err := f.db.QueryRow(`SELECT actor, target_type, target_id, detail, created_at FROM audit_logs
				WHERE action = 'task.resume' AND target_id = ?`, taskID).Scan(&actor, &targetType, &targetID, &detail, &createdAt); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(detail), &got); err != nil {
				t.Fatal(err)
			}
			to := "queued"
			if decision == "abort" {
				to = "cancelled"
			}
			want := map[string]any{"executionId": id, "blockId": input.BlockID, "decision": decision,
				"headSha": input.HeadSHA, "from": "blocked", "to": to}
			if actor != "owner" || targetType != "task" || targetID != taskID || !reflect.DeepEqual(got, want) {
				t.Fatalf("audit=%s/%s/%s %s", actor, targetType, targetID, detail)
			}
			if _, err := ParseTimestamp(createdAt); err != nil {
				t.Fatal(err)
			}
			requireSavedDecision(t, f, input)
			before := resumeState(t, f, id)
			status, body = requestResume(t, f, taskID, input)
			assertAuthError(t, status, body, 409, Conflict)
			requireResumeUnchanged(t, f, id, before)
		})
	}
}

func TestResumeContinueWaitsForNormalClaimCapacity(t *testing.T) {
	f, token, id, input := resumeFixture(t, "gate1_test_change")
	// A blocked attempt consumes no slot, so another task can run in its Project.
	otherA := createClaimTask(t, f, "project-a", "resume-capacity-a")
	status, body := requestClaim(t, f, token)
	claimA := requireClaim(t, status, body)
	if claimA.Execution.TaskID != otherA.ID {
		t.Fatalf("other Project A task was not claimed: %+v", claimA.Execution)
	}
	createClaimTask(t, f, "project-b", "resume-capacity-b")
	status, body = requestClaim(t, f, token)
	claimB := requireClaim(t, status, body)
	status, body = requestResume(t, f, executionTaskID(t, f, id), input)
	requireResumeSuccess(t, status, body)
	status, body = requestClaim(t, f, token)
	requireResumeSuccess(t, status, body) // Global capacity is full.
	status, body = postResult(t, f, token, claimB.Execution.ID, resultBody("claimed", "cancelled"))
	requireResumeSuccess(t, status, body)
	status, body = requestClaim(t, f, token)
	requireResumeSuccess(t, status, body) // Project A is still occupied.
	var state, point string
	if err := f.db.QueryRow(`SELECT status, resume_point FROM executions WHERE id = ?`, id).Scan(&state, &point); err != nil || state != "pending" || point != "gate2" {
		t.Fatalf("waiting state=%s point=%s err=%v", state, point, err)
	}
	status, body = postResult(t, f, token, claimA.Execution.ID, resultBody("claimed", "cancelled"))
	requireResumeSuccess(t, status, body)
	status, body = requestClaim(t, f, token)
	resumed := requireClaim(t, status, body)
	if resumed.Execution.ID != id || resumed.Execution.ResumePoint == nil || *resumed.Execution.ResumePoint != "gate2" {
		t.Fatalf("resumed claim=%+v", resumed.Execution)
	}
}

func TestResumeRollsBackDecisionStateAndRecordsOnStorageFailure(t *testing.T) {
	for _, decision := range []string{"continue", "abort"} {
		triggers := []string{"BEFORE UPDATE OF decision ON execution_blocks", "BEFORE UPDATE OF status ON executions",
			"BEFORE UPDATE OF status ON tasks", "BEFORE INSERT ON execution_events", "BEFORE INSERT ON audit_logs"}
		if decision == "continue" {
			triggers = append(triggers, "BEFORE UPDATE OF resume_point ON executions")
		} else {
			triggers = append(triggers, "BEFORE UPDATE OF error_class ON executions", "BEFORE UPDATE OF finished_at ON executions")
		}
		for i, trigger := range triggers {
			t.Run(fmt.Sprintf("%s/%d", decision, i), func(t *testing.T) {
				f, _, id, input := resumeFixture(t, "gate1_test_change")
				input.Decision = decision
				before := resumeState(t, f, id)
				if _, err := f.db.Exec(`CREATE TRIGGER reject_resume ` + trigger + ` BEGIN SELECT RAISE(ABORT, 'resume unavailable'); END`); err != nil {
					t.Fatal(err)
				}
				status, body := requestResume(t, f, executionTaskID(t, f, id), input)
				assertAuthError(t, status, body, 500, Internal)
				if strings.Contains(string(body), "resume unavailable") {
					t.Fatal("internal storage error exposed")
				}
				requireResumeUnchanged(t, f, id, before)
			})
		}
	}
}

func TestResumeConcurrentDecisionsAreRecordedOnce(t *testing.T) {
	f, _, id, input := resumeFixture(t, "gate1_test_change")
	taskID := executionTaskID(t, f, id)
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan int, 12)
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			decision := input
			if i%2 == 0 {
				decision.Decision = "abort"
			}
			status, body := requestResume(t, f, taskID, decision)
			if status != 204 && status != 409 {
				t.Errorf("concurrent resume=%d %s", status, body)
			}
			results <- status
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for status := range results {
		if status == 204 {
			success++
		}
	}
	after := resumeState(t, f, id)
	if success != 1 || after.Audits != 1 || after.N != 1 {
		t.Fatalf("success=%d state=%+v", success, after)
	}
	var decision, state string
	var point sql.NullString
	if err := f.db.QueryRow(`SELECT b.decision, e.status, e.resume_point FROM executions e
		JOIN execution_blocks b ON b.execution_id = e.id WHERE e.id = ?`, id).Scan(&decision, &state, &point); err != nil {
		t.Fatal(err)
	}
	if decision == "continue" && (state != "pending" || after.Task != "queued" || point.String != "gate2") ||
		decision == "abort" && (state != "cancelled" || after.Task != "cancelled" || point.Valid) {
		t.Fatalf("decision=%s state=%s task=%s point=%v", decision, state, after.Task, point)
	}
}

func TestResumeRejectsForeignMissingStaleAndNonBlockedTargets(t *testing.T) {
	for _, name := range []string{"missing-task", "missing-block", "foreign-block", "old-execution", "task-state", "execution-state", "decided"} {
		t.Run(name, func(t *testing.T) {
			f, _, id, input := resumeFixture(t, "gate1_test_change")
			taskID := executionTaskID(t, f, id)
			want, code := 409, Conflict
			switch name {
			case "missing-task":
				taskID, want, code = "task-999999", 404, NotFound
			case "missing-block":
				input.BlockID, want, code = ulid.Make().String(), 404, NotFound
			case "foreign-block":
				other := createClaimTask(t, f, "project-a", "foreign-resume")
				taskID, want, code = other.ID, 404, NotFound
			case "old-execution":
				if _, err := f.db.Exec(`INSERT INTO executions (id, task_id, seq, mode, status, created_at)
					SELECT ?, task_id, seq+1, 'continue', 'blocked', created_at FROM executions WHERE id = ?`, ulid.Make().String(), id); err != nil {
					t.Fatal(err)
				}
			case "task-state":
				if _, err := f.db.Exec(`UPDATE tasks SET status = 'queued' WHERE id = ?`, taskID); err != nil {
					t.Fatal(err)
				}
			case "execution-state":
				if _, err := f.db.Exec(`UPDATE executions SET status = 'pending' WHERE id = ?`, id); err != nil {
					t.Fatal(err)
				}
			case "decided":
				if _, err := f.db.Exec(`UPDATE execution_blocks SET decision = 'abort', decided_at = '2026-10-28T00:00:00Z' WHERE id = ?`, input.BlockID); err != nil {
					t.Fatal(err)
				}
			}
			before := resumeState(t, f, id)
			status, body := requestResume(t, f, taskID, input)
			assertAuthError(t, status, body, want, code)
			requireResumeUnchanged(t, f, id, before)
		})
	}
}

func TestResumeValidatesJSONIDsDecisionAndSHA(t *testing.T) {
	f, _, id, input := resumeFixture(t, "gate1_test_change")
	taskID := executionTaskID(t, f, id)
	valid := `{"blockId":"` + input.BlockID + `","decision":"continue","headSha":"` + input.HeadSHA + `"}`
	for _, body := range []string{"", "null", "[]", "{}", valid + `{}`, strings.Replace(valid, `"continue"`, `null`, 1),
		strings.Replace(valid, `"continue"`, `"unknown"`, 1), strings.Replace(valid, input.BlockID, "bad-id", 1),
		strings.Replace(valid, input.HeadSHA, "", 1), strings.Replace(valid, input.HeadSHA, "bad-sha", 1),
		strings.Replace(valid, `"headSha"`, `"unknown"`, 1), strings.Replace(valid, `"continue"`, `42`, 1)} {
		t.Run(body, func(t *testing.T) {
			before := resumeState(t, f, id)
			status, response := requestResumeJSON(t, f, taskID, body, nil)
			assertAuthError(t, status, response, 400, ValidationFailed)
			requireResumeUnchanged(t, f, id, before)
		})
	}
	status, body := requestResume(t, f, "invalid", input)
	assertAuthError(t, status, body, 400, ValidationFailed)
}

func TestResumeRequiresOwnerSessionCSRFAndBoundary(t *testing.T) {
	f, token, id, input := resumeFixture(t, "gate1_test_change")
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session", "csrf", "origin", "missing-origin", "host", "runner"} {
		t.Run(name, func(t *testing.T) {
			before := resumeState(t, f, id)
			status, body := requestResumeJSON(t, f, executionTaskID(t, f, id), string(data), func(r *http.Request) {
				switch name {
				case "session":
					r.Header.Del("Cookie")
				case "csrf":
					r.Header.Del("X-CSRF-Token")
				case "origin":
					r.Header.Set("Origin", "http://evil.example")
				case "missing-origin":
					r.Header.Del("Origin")
				case "host":
					r.Host = "evil.example"
				case "runner":
					r.Header.Del("Cookie")
					r.Header.Set("Authorization", "Bearer "+token)
				}
			})
			want, code := 403, Forbidden
			if name == "session" {
				want, code = 401, Unauthenticated
			}
			assertAuthError(t, status, body, want, code)
			requireResumeUnchanged(t, f, id, before)
		})
	}
}
