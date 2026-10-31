package api

import (
	"database/sql"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func requestRetry(t *testing.T, f *authFixture, id, body string, alter func(*http.Request)) (int, []byte) {
	t.Helper()
	return f.request(t, "POST", "/api/tasks/"+id+"/retry", true, f.csrf, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		r.Body = io.NopCloser(strings.NewReader(body))
		if alter != nil {
			alter(r)
		}
	})
}

func retryFixture(t *testing.T, to string) (*authFixture, string, string) {
	t.Helper()
	from := "running"
	if to == "succeeded" {
		from = "publishing"
	}
	f, token, id, _ := resultFixture(t, from)
	status, body := postResult(t, f, token, id, resultBody(from, to))
	if status != 204 {
		t.Fatalf("prepare retry: %d %s", status, body)
	}
	return f, token, id
}

// Snapshot all columns so retry cannot silently clear metadata on an old attempt.
func retryExecutionSnapshot(t *testing.T, db *sql.DB, id string) []any {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM executions WHERE id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	values := make([]any, len(columns))
	dest := make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	if !rows.Next() {
		t.Fatalf("missing Execution %s: %v", id, rows.Err())
	}
	if err := rows.Scan(dest...); err != nil {
		t.Fatal(err)
	}
	return values
}

func requireRetryPending(t *testing.T, f *authFixture, taskID, mode string, wantSeq int) string {
	t.Helper()
	var id, status, taskStatus, savedMode, created string
	var seq, count, unpinned int
	err := f.db.QueryRow(`SELECT e.id, e.seq, e.mode, e.status, e.created_at, t.status,
		(SELECT count(*) FROM executions WHERE task_id = t.id),
		e.runner_id IS NULL AND e.role IS NULL AND e.harness_id IS NULL AND e.profile_id IS NULL
		AND e.profile_version IS NULL AND e.profile_commit_sha IS NULL AND e.claimed_at IS NULL
		AND e.worktree_id IS NULL AND e.branch_name IS NULL AND e.base_sha IS NULL
		AND e.resume_point IS NULL AND e.pr_number IS NULL
		AND e.pr_url IS NULL AND e.cancel_requested_at IS NULL AND e.finished_at IS NULL
		FROM tasks t JOIN executions e ON e.task_id = t.id
		WHERE t.id = ? ORDER BY e.seq DESC LIMIT 1`, taskID).
		Scan(&id, &seq, &savedMode, &status, &created, &taskStatus, &count, &unpinned)
	if err != nil || seq != wantSeq || count != wantSeq || status != "pending" || taskStatus != "queued" || savedMode != mode || unpinned != 1 {
		t.Fatalf("retry id=%s seq=%d count=%d mode=%s status=%s task=%s unpinned=%d err=%v", id, seq, count, savedMode, status, taskStatus, unpinned, err)
	}
	if err := ValidateID("id", id, ULID); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTimestamp(created); err != nil {
		t.Fatal(err)
	}
	return id
}

func requireRetrySuccess(t *testing.T, status int, body []byte) {
	t.Helper()
	if status != 204 || len(body) != 0 {
		t.Fatalf("retry=%d %s", status, body)
	}
}

func TestRetryFailedCreatesNextPendingExecutionAndRecordsMode(t *testing.T) {
	for _, mode := range []string{"continue", "fresh"} {
		t.Run(mode, func(t *testing.T) {
			f, token, oldID := retryFixture(t, "failed")
			taskID := executionTaskID(t, f, oldID)
			before := retryExecutionSnapshot(t, f.db, oldID)
			for seq := 2; seq <= 3; seq++ {
				status, body := requestRetry(t, f, taskID, `{"mode":"`+mode+`"}`, nil)
				requireRetrySuccess(t, status, body)
				id := requireRetryPending(t, f, taskID, mode, seq)
				if id == oldID || !reflect.DeepEqual(retryExecutionSnapshot(t, f.db, oldID), before) {
					t.Fatal("retry changed the previous Execution")
				}
				if seq == 2 {
					status, body = requestClaim(t, f, token)
					claim := requireClaim(t, status, body)
					if claim.Execution.ID != id || claim.Execution.Seq != seq || claim.Execution.Mode != mode {
						t.Fatalf("retry not delivered by claim: %+v", claim.Execution)
					}
					status, body = postResult(t, f, token, id, resultBody("claimed", "failed"))
					if status != 204 {
						t.Fatalf("fail second attempt: %d %s", status, body)
					}
				}
			}
		})
	}
}

func TestRetryAcceptsAllRetriableStatesAndPreservesEvidence(t *testing.T) {
	for _, state := range []string{"timed_out", "cancelled", "succeeded", "blocked", "interrupted"} {
		t.Run(state, func(t *testing.T) {
			to := state
			if state == "interrupted" {
				to = "failed" // Runner cannot report interrupted; recovery owns it.
			}
			f, _, id := retryFixture(t, to)
			if state == "interrupted" {
				if _, err := f.db.Exec(`UPDATE executions SET status = 'interrupted', error_class = 'interrupted' WHERE id = ?`, id); err != nil {
					t.Fatal(err)
				}
			}
			taskID := executionTaskID(t, f, id)
			before := retryExecutionSnapshot(t, f.db, id)
			var worktree, block string
			if err := f.db.QueryRow(`SELECT w.status, coalesce((SELECT json_object('id', id, 'cause', cause,
				'headSha', head_sha, 'detail', detail, 'decision', decision, 'decidedAt', decided_at)
				FROM execution_blocks WHERE execution_id = e.id), '')
				FROM executions e JOIN worktrees w ON w.id = e.worktree_id WHERE e.id = ?`, id).Scan(&worktree, &block); err != nil {
				t.Fatal(err)
			}
			status, body := requestRetry(t, f, taskID, `{"mode":"continue"}`, nil)
			requireRetrySuccess(t, status, body)
			requireRetryPending(t, f, taskID, "continue", 2)
			if !reflect.DeepEqual(retryExecutionSnapshot(t, f.db, id), before) {
				t.Fatal("previous Execution was changed")
			}
			var afterWorktree, afterBlock string
			if err := f.db.QueryRow(`SELECT w.status, coalesce((SELECT json_object('id', id, 'cause', cause,
				'headSha', head_sha, 'detail', detail, 'decision', decision, 'decidedAt', decided_at)
				FROM execution_blocks WHERE execution_id = e.id), '')
				FROM executions e JOIN worktrees w ON w.id = e.worktree_id WHERE e.id = ?`, id).Scan(&afterWorktree, &afterBlock); err != nil {
				t.Fatal(err)
			}
			if afterWorktree != worktree || afterBlock != block {
				t.Fatal("retry changed worktree or block evidence")
			}
		})
	}
}

func TestRetryRunningAndOtherForbiddenStatesReturn409WithoutWrites(t *testing.T) {
	for _, state := range []struct{ task, execution string }{
		{"running", "running"}, {"running", "claimed"}, {"running", "preparing"},
		{"verifying", "verifying"}, {"verifying", "publishing"}, {"queued", "pending"},
		{"received", "pending"}, {"planning", "pending"}, {"waiting_approval", "pending"},
		{"failed", "running"}, // Fail closed when the latest Execution disagrees.
	} {
		t.Run(state.task+"_"+state.execution, func(t *testing.T) {
			f, _, id, _ := resultFixture(t, "running")
			taskID := executionTaskID(t, f, id)
			if _, err := f.db.Exec(`UPDATE executions SET status = ? WHERE id = ?`, state.execution, id); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, state.task, taskID); err != nil {
				t.Fatal(err)
			}
			before := retryExecutionSnapshot(t, f.db, id)
			status, body := requestRetry(t, f, taskID, `{"mode":"fresh"}`, nil)
			assertAuthError(t, status, body, 409, Conflict)
			if !reflect.DeepEqual(retryExecutionSnapshot(t, f.db, id), before) || projectCount(t, f, "executions") != 1 {
				t.Fatal("rejected retry changed Executions")
			}
			var saved string
			if err := f.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&saved); err != nil || saved != state.task {
				t.Fatalf("task=%s err=%v", saved, err)
			}
		})
	}
}

func TestRetryConcurrentRequestsCreateOneExecution(t *testing.T) {
	f, _, id := retryFixture(t, "failed")
	taskID := executionTaskID(t, f, id)
	var wg sync.WaitGroup
	results := make(chan int, 12)
	start := make(chan struct{})
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, body := requestRetry(t, f, taskID, `{"mode":"continue"}`, nil)
			if status >= 300 && status != 409 {
				t.Errorf("concurrent retry: %d %s", status, body)
			}
			results <- status
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for status := range results {
		if status >= 200 && status < 300 {
			success++
		}
		if status == 409 {
			conflicts++
		}
	}
	if success != 1 || conflicts != 11 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
	requireRetryPending(t, f, taskID, "continue", 2)
}

func TestRetryRollsBackExecutionAndTaskOnStorageFailure(t *testing.T) {
	f, _, id := retryFixture(t, "failed")
	taskID := executionTaskID(t, f, id)
	before := retryExecutionSnapshot(t, f.db, id)
	for _, trigger := range []string{"BEFORE INSERT ON executions", "BEFORE UPDATE OF status ON tasks"} {
		if _, err := f.db.Exec(`CREATE TRIGGER reject_retry ` + trigger + ` BEGIN SELECT RAISE(ABORT, 'retry unavailable'); END`); err != nil {
			t.Fatal(err)
		}
		status, body := requestRetry(t, f, taskID, `{"mode":"fresh"}`, nil)
		assertAuthError(t, status, body, 500, Internal)
		if strings.Contains(string(body), "retry unavailable") {
			t.Fatal("storage error was disclosed")
		}
		var taskStatus string
		if err := f.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&taskStatus); err != nil || taskStatus != "failed" {
			t.Fatalf("task=%s err=%v", taskStatus, err)
		}
		if projectCount(t, f, "executions") != 1 || !reflect.DeepEqual(retryExecutionSnapshot(t, f.db, id), before) {
			t.Fatal("partial retry survived rollback")
		}
		if _, err := f.db.Exec(`DROP TRIGGER reject_retry`); err != nil {
			t.Fatal(err)
		}
	}
	status, body := requestRetry(t, f, taskID, `{"mode":"fresh"}`, nil)
	requireRetrySuccess(t, status, body)
	requireRetryPending(t, f, taskID, "fresh", 2)
}

func TestRetryValidatesModeJSONAndTaskID(t *testing.T) {
	f, _, id := retryFixture(t, "failed")
	taskID := executionTaskID(t, f, id)
	for _, tc := range []struct{ body, field, reason string }{
		{`{}`, "mode", "required"}, {`{"mode":null}`, "mode", "required"}, {`{"mode":""}`, "mode", "required"},
		{`{"mode":"unknown"}`, "mode", "invalid"}, {`{"mode":"FRESH"}`, "mode", "invalid"}, {`{"mode":" fresh "}`, "mode", "invalid"},
		{`{"mode":1}`, "body", "invalid_json"}, {`{"mode":"fresh","seq":5}`, "body", "invalid_json"},
		{`null`, "body", "invalid_json"}, {`[]`, "body", "invalid_json"}, {``, "body", "invalid_json"},
		{`{"mode":"fresh"} {}`, "body", "invalid_json"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			status, body := requestRetry(t, f, taskID, tc.body, nil)
			requireProjectError(t, status, body, 400, ValidationFailed, tc.field, tc.reason)
		})
	}
	status, body := requestRetry(t, f, "invalid", `{"mode":"fresh"}`, nil)
	requireProjectError(t, status, body, 400, ValidationFailed, "id", "invalid_format")
	status, body = requestRetry(t, f, "task-999999", `{"mode":"continue"}`, nil)
	assertAuthError(t, status, body, 404, NotFound)
	if projectCount(t, f, "executions") != 1 {
		t.Fatal("invalid input created an Execution")
	}
}

func TestRetryRequiresOwnerSessionCSRFAndBoundary(t *testing.T) {
	f, token, id := retryFixture(t, "failed")
	taskID := executionTaskID(t, f, id)
	for _, name := range []string{"session", "csrf", "origin", "missing-origin", "host", "runner"} {
		t.Run(name, func(t *testing.T) {
			status, body := requestRetry(t, f, taskID, `{"mode":"continue"}`, func(r *http.Request) {
				switch name {
				case "session":
					r.Header.Del("Cookie")
				case "csrf":
					r.Header.Del("X-CSRF-Token")
				case "origin":
					r.Header.Set("Origin", "http://other.invalid")
				case "missing-origin":
					r.Header.Del("Origin")
				case "host":
					r.Host = "other.invalid"
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
			if projectCount(t, f, "executions") != 1 {
				t.Fatal("unauthorized retry created an Execution")
			}
		})
	}
}
