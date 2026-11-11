package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

func resultFixture(t *testing.T, from string) (*authFixture, string, string, string) {
	t.Helper()
	f, token, _ := claimFixture(t)
	createClaimTask(t, f, "project-a", "result")
	status, body := requestClaim(t, f, token)
	claim := requireClaim(t, status, body)
	id := claim.Execution.ID
	current := "claimed"
	for _, next := range []string{"preparing", "working", "checking", "publishing"} {
		if current == from {
			break
		}
		mapped, _ := task.TaskStatusForExecution(model.ExecutionStatus(current))
		if err := task.TransitionExecution(context.Background(), f.db, task.Transition{
			TaskID: claim.Execution.TaskID, ExecutionID: id, FromTask: mapped,
			FromExecution: model.ExecutionStatus(current), ToExecution: model.ExecutionStatus(next),
		}); err != nil {
			t.Fatal(err)
		}
		current = next
	}
	return f, token, id, claim.Execution.TaskID
}

func postResult(t *testing.T, f *authFixture, token, id string, input any) (int, []byte) {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return f.request(t, "POST", "/api/executions/"+id+"/result", false, "", func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Body = io.NopCloser(strings.NewReader(string(data)))
	})
}

func resultBody(from, to string) map[string]any {
	input := map[string]any{"fromStatus": from, "status": to}
	if to == "succeeded" {
		input["stage"] = "publish"
		input["usage"] = map[string]any{"wallTimeMs": 0}
	}
	if to == "failed" {
		input["errorClass"] = "work_failed"
		input["errorMessage"] = "Harness exited"
	}
	return input
}

type resultSnapshot struct {
	Execution, Task          string
	Finished                 sql.NullString
	Requests, Events, Audits int
	UsageRecords             int
}

func readResultSnapshot(t *testing.T, f *authFixture, id string) resultSnapshot {
	t.Helper()
	var s resultSnapshot
	err := f.db.QueryRow(`SELECT e.status, t.status, e.finished_at,
		(SELECT count(*) FROM notification_requests WHERE execution_id = e.id),
		(SELECT count(*) FROM execution_events WHERE execution_id = e.id),
		(SELECT count(*) FROM audit_logs WHERE action = 'execution.result' AND target_id = e.id),
		(SELECT count(*) FROM usage_records WHERE execution_id = e.id)
		FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, id).
		Scan(&s.Execution, &s.Task, &s.Finished, &s.Requests, &s.Events, &s.Audits, &s.UsageRecords)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestResultRejectsEveryUnlistedTransitionWithoutWrites(t *testing.T) {
	f, token, id, _ := resultFixture(t, "claimed")
	states := []string{"pending", "claimed", "preparing", "working", "checking", "publishing", "succeeded", "blocked", "failed", "interrupted", "cancelled", "timed_out", "unknown"}
	// Explicit expected Runner edges, independent of the implementation table.
	// blocked had only development causes (Gate, Context, Git), which moved to
	// software-engineer (spec 19 3.11); the Runner can no longer report it.
	allowed := map[string][]string{
		// Spec 02 5.2: without a prepare stage a claimed Execution starts
		// working. Rejecting prepare for a Profile without it is spec 04 3.4.1.
		"claimed":    {"preparing", "working", "failed", "timed_out", "cancelled"},
		"preparing":  {"working", "failed", "timed_out", "cancelled"},
		"working":    {"checking", "failed", "timed_out", "cancelled"},
		"checking":   {"publishing", "failed", "timed_out", "cancelled"},
		"publishing": {"succeeded", "failed", "timed_out", "cancelled"},
	}
	for _, from := range states[:len(states)-1] {
		mapped, _ := task.TaskStatusForExecution(model.ExecutionStatus(from))
		if _, err := f.db.Exec(`UPDATE executions SET status = ? WHERE id = ?`, from, id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`UPDATE tasks SET status = ? WHERE id = (SELECT task_id FROM executions WHERE id = ?)`, mapped, id); err != nil {
			t.Fatal(err)
		}
		for _, to := range states {
			valid := false
			for _, next := range allowed[from] {
				valid = valid || next == to
			}
			if valid {
				continue
			}
			t.Run(from+"_to_"+to, func(t *testing.T) {
				before := readResultSnapshot(t, f, id)
				status, body := postResult(t, f, token, id, resultBody(from, to))
				assertAuthError(t, status, body, 409, Conflict)
				if after := readResultSnapshot(t, f, id); after != before {
					t.Fatalf("rejected result mutated DB: before=%+v after=%+v", before, after)
				}
			})
		}
	}
}

func TestResultConcurrentAndReplayedStopsRequestNotificationOnce(t *testing.T) {
	for _, target := range []string{"succeeded", "failed", "timed_out", "cancelled"} {
		t.Run(target, func(t *testing.T) {
			from := "working"
			if target == "succeeded" {
				from = "publishing"
			}
			f, token, id, _ := resultFixture(t, from)
			before := readResultSnapshot(t, f, id)
			input := resultBody(from, target)
			input["harnessSessionId"] = "session-1"
			results := make(chan int, 8)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					status, body := postResult(t, f, token, id, input)
					if status != 204 {
						assertAuthError(t, status, body, 409, Conflict)
					}
					results <- status
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			counts := map[int]int{}
			for status := range results {
				counts[status]++
			}
			if counts[204] != 1 || counts[409] != 7 {
				t.Fatalf("parallel result statuses=%v", counts)
			}
			after := readResultSnapshot(t, f, id)
			wantTask, _ := task.TaskStatusForExecution(model.ExecutionStatus(target))
			wantRequests, wantEvents := 1, 1
			if target == "cancelled" {
				wantRequests = 0 // Spec 12 explicitly excludes cancellation.
			}
			if after.Execution != target || after.Task != string(wantTask) ||
				after.Requests != wantRequests || after.Events != before.Events+wantEvents || after.Audits != 1 ||
				!after.Finished.Valid {
				t.Fatalf("saved stop=%+v", after)
			}
			wantUsage := 0
			if target == "succeeded" {
				wantUsage = 1
			}
			if after.UsageRecords != wantUsage {
				t.Fatalf("usage rows=%d; want %d", after.UsageRecords, wantUsage)
			}
			if after.Finished.Valid {
				if _, err := ParseTimestamp(after.Finished.String); err != nil {
					t.Fatal(err)
				}
			}
			var session string
			if err := f.db.QueryRow(`SELECT work_session_id FROM executions WHERE id = ?`, id).Scan(&session); err != nil || session != "session-1" {
				t.Fatalf("saved session: %s err=%v", session, err)
			}
			// Reopen the actual SQLite file and rebuild routing/authentication, as
			// a process restart would. Runner tokens and deduplication must persist.
			var seq int
			var name, filename string
			if err := f.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &filename); err != nil {
				t.Fatal(err)
			}
			f.server.Close()
			if err := f.db.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			f.db, err = store.Open(context.Background(), filename)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { f.db.Close() })
			f.server = httptest.NewUnstartedServer(nil)
			f.auth, err = NewOwnerAuth(f.db, nil, f.server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			f.server.Config.Handler = NewHandler(f.db, nil, f.auth)
			f.server.Start()
			t.Cleanup(f.server.Close)
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 409, Conflict)
			if replay := readResultSnapshot(t, f, id); replay != after {
				t.Fatalf("restart/replay mutated DB: before=%+v after=%+v", after, replay)
			}
			var sent int
			if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE type = 'notification_sent'`).Scan(&sent); err != nil || sent != 0 {
				t.Fatalf("request falsely recorded as delivery: %d err=%v", sent, err)
			}
		})
	}
}

func TestResultRollsBackAllWritesOnStorageFailure(t *testing.T) {
	f, token, id, _ := resultFixture(t, "working")
	before := readResultSnapshot(t, f, id)
	for i, trigger := range []string{
		`BEFORE UPDATE OF work_session_id ON executions`,
		`BEFORE UPDATE OF status ON tasks`,
		`BEFORE INSERT ON notification_requests`,
		`BEFORE INSERT ON execution_events WHEN json_extract(NEW.payload, '$.reason') = 'runner_result'`,
		`BEFORE INSERT ON audit_logs WHEN NEW.action = 'execution.result'`,
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, err := f.db.Exec(`CREATE TRIGGER reject_result ` + trigger + ` BEGIN SELECT RAISE(ABORT, 'result storage unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			input := resultBody("working", "failed")
			input["harnessSessionId"] = "session-1"
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 500, Internal)
			if after := readResultSnapshot(t, f, id); after != before {
				t.Fatalf("partial result survived failure: before=%+v after=%+v", before, after)
			}
			var session sql.NullString
			if err := f.db.QueryRow(`SELECT work_session_id FROM executions WHERE id = ?`, id).Scan(&session); err != nil || session.Valid {
				t.Fatalf("metadata survived failure: %+v err=%v", session, err)
			}
			if _, err := f.db.Exec(`DROP TRIGGER reject_result`); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResultRequiresClaimOwnerAndRunnerAuthentication(t *testing.T) {
	f, token, id, _ := resultFixture(t, "claimed")
	other, err := runnerauth.Register(context.Background(), f.db, "other-runner", "Other", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := readResultSnapshot(t, f, id)
	status, body := postResult(t, f, other, id, resultBody("claimed", "preparing"))
	assertAuthError(t, status, body, 403, Forbidden)
	var denials int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action = 'auth.reject'
		AND json_extract(detail, '$.reason') = 'execution_runner_mismatch'`).Scan(&denials); err != nil || denials != 1 {
		t.Fatalf("ownership denials=%d err=%v", denials, err)
	}
	status, body = postResult(t, f, "invalid", id, resultBody("claimed", "preparing"))
	assertAuthError(t, status, body, 401, Unauthenticated)
	status, body = f.request(t, "POST", "/api/executions/"+id+"/result", true, f.csrf, nil)
	assertAuthError(t, status, body, 403, Forbidden)
	if after := readResultSnapshot(t, f, id); after != before {
		t.Fatalf("denial mutated execution: before=%+v after=%+v", before, after)
	}
	// Project assignment is not a substitute for the Execution claim.
	if _, err := f.db.Exec(`UPDATE executions SET runner_id = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	status, body = postResult(t, f, token, id, resultBody("claimed", "preparing"))
	assertAuthError(t, status, body, 403, Forbidden)
}

func TestResultProgressAndStaleReports(t *testing.T) {
	f, token, id, _ := resultFixture(t, "claimed")
	from := "claimed"
	for _, to := range []string{"preparing", "working", "checking", "publishing"} {
		input := resultBody(from, to)
		status, body := postResult(t, f, token, id, input)
		if status != 204 || len(body) != 0 {
			t.Fatalf("progress %s -> %s: %d %s", from, to, status, body)
		}
		before := readResultSnapshot(t, f, id)
		wantTask, _ := task.TaskStatusForExecution(model.ExecutionStatus(to))
		if before.Execution != to || before.Task != string(wantTask) || before.Requests != 0 {
			t.Fatalf("progress=%+v", before)
		}
		status, body = postResult(t, f, token, id, input)
		assertAuthError(t, status, body, 409, Conflict)
		if after := readResultSnapshot(t, f, id); after != before {
			t.Fatalf("stale progress mutated DB: %+v", after)
		}
		from = to
	}
	var started string
	if err := f.db.QueryRow(`SELECT started_at FROM executions WHERE id = ?`, id).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTimestamp(started); err != nil {
		t.Fatal(err)
	}
}

func TestResultValidationLeavesStateAndNotificationRequestsUntouched(t *testing.T) {
	f, token, id, _ := resultFixture(t, "working")
	before := readResultSnapshot(t, f, id)
	for _, tc := range []struct {
		name   string
		input  map[string]any
		status int
	}{
		{"missing class", map[string]any{"status": "failed"}, 400},
		{"unknown class", map[string]any{"status": "failed", "errorClass": "unknown"}, 400},
		{"wrong timeout class", map[string]any{"status": "timed_out", "errorClass": "cancelled"}, 400},
		{"unknown field", map[string]any{"status": "checking", "runnerId": "claim-runner"}, 400},
		// The development result fields moved to software-engineer (spec 19 3.11).
		{"removed block", map[string]any{"status": "checking", "block": map[string]any{"cause": "git_state", "headSha": strings.Repeat("a", 40)}}, 400},
		{"removed SHA", map[string]any{"status": "checking", "verifiedSha": strings.Repeat("a", 40)}, 400},
		{"removed PR", map[string]any{"status": "checking", "prNumber": 1}, 400},
		{"removed worktree", map[string]any{"status": "checking", "worktreeId": ulid.Make().String()}, 400},
		{"removed gate results", map[string]any{"status": "checking", "gateResults": []any{}}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.input["fromStatus"] = "working"
			status, body := postResult(t, f, token, id, tc.input)
			code := ValidationFailed
			if tc.status == 422 {
				code = PreconditionFailed
			}
			assertAuthError(t, status, body, tc.status, code)
			if after := readResultSnapshot(t, f, id); after != before {
				t.Fatalf("invalid metadata mutated DB: %+v", after)
			}
		})
	}
}

func TestResultRejectsHistoricalExecutionAndInconsistentTask(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(fmt.Sprint(historical), func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "working")
			if historical {
				if _, err := f.db.Exec(`INSERT INTO executions (id, task_id, seq, mode, status, created_at)
					SELECT ?, task_id, seq+1, 'continue', 'pending', created_at FROM executions WHERE id = ?`, ulid.Make().String(), id); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.db.Exec(`UPDATE tasks SET status = 'queued' WHERE id = (SELECT task_id FROM executions WHERE id = ?)`, id); err != nil {
					t.Fatal(err)
				}
			}
			before := readResultSnapshot(t, f, id)
			status, body := postResult(t, f, token, id, resultBody("working", "failed"))
			assertAuthError(t, status, body, 409, Conflict)
			if after := readResultSnapshot(t, f, id); after != before {
				t.Fatalf("stale execution/task changed: %+v", after)
			}
		})
	}
}

func TestInterruptionRequestsNotificationOnce(t *testing.T) {
	f, _, id, _ := resultFixture(t, "preparing")
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	execution, err := store.GetContextExecution(context.Background(), tx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := task.TransitionExecutionTx(context.Background(), tx, task.Transition{
		TaskID: execution.TaskID, ExecutionID: id, FromTask: execution.TaskStatus,
		FromExecution: execution.Status, ToExecution: model.ExecutionInterrupted,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if after := readResultSnapshot(t, f, id); after.Requests != 1 || after.Execution != "interrupted" {
		t.Fatalf("transition effects=%+v", after)
	}
}
