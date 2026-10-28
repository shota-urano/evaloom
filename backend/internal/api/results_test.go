package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	for _, next := range []string{"preparing", "running", "verifying", "publishing"} {
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
	worktree := ulid.Make().String()
	if _, err := f.db.Exec(`INSERT INTO worktrees (id, project_id, task_id, runner_id, path,
		branch_name, base_sha, status, setup_status, copied_files, created_at)
		VALUES (?, 'project-a', ?, 'claim-runner', '/worktrees/result', 'result', ?, 'active', 'succeeded', '[]', '2026-10-27T00:00:00Z')`,
		worktree, claim.Execution.TaskID, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET worktree_id = ? WHERE id = ?`, worktree, id); err != nil {
		t.Fatal(err)
	}
	return f, token, id, worktree
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
		input["usage"] = map[string]any{"wallTimeMs": 0}
	}
	if to == "blocked" {
		input["block"] = map[string]any{"cause": "git_state", "headSha": strings.Repeat("b", 40), "detail": map[string]any{"reason": "dirty"}}
	}
	if to == "failed" {
		input["errorClass"] = "execution_failed"
		input["errorMessage"] = "Harness exited"
	}
	return input
}

type resultSnapshot struct {
	Execution, Task, Worktree        string
	Finished                         sql.NullString
	Requests, Events, Audits, Blocks int
	UsageRecords                     int
}

func readResultSnapshot(t *testing.T, f *authFixture, id string) resultSnapshot {
	t.Helper()
	var s resultSnapshot
	err := f.db.QueryRow(`SELECT e.status, t.status, w.status, e.finished_at,
		(SELECT count(*) FROM notification_requests WHERE execution_id = e.id),
		(SELECT count(*) FROM execution_events WHERE execution_id = e.id),
		(SELECT count(*) FROM audit_logs WHERE action = 'execution.result' AND target_id = e.id),
		(SELECT count(*) FROM execution_blocks WHERE execution_id = e.id),
		(SELECT count(*) FROM usage_records WHERE execution_id = e.id)
		FROM executions e JOIN tasks t ON t.id = e.task_id JOIN worktrees w ON w.id = e.worktree_id WHERE e.id = ?`, id).
		Scan(&s.Execution, &s.Task, &s.Worktree, &s.Finished, &s.Requests, &s.Events, &s.Audits, &s.Blocks, &s.UsageRecords)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestResultRejectsEveryUnlistedTransitionWithoutWrites(t *testing.T) {
	f, token, id, _ := resultFixture(t, "claimed")
	states := []string{"pending", "claimed", "preparing", "running", "verifying", "publishing", "succeeded", "blocked", "failed", "interrupted", "cancelled", "timed_out", "unknown"}
	// Explicit expected Runner edges, independent of the implementation table.
	allowed := map[string][]string{
		"claimed":    {"preparing", "failed", "timed_out", "cancelled", "blocked"},
		"preparing":  {"running", "failed", "timed_out", "cancelled", "blocked"},
		"running":    {"verifying", "failed", "timed_out", "cancelled", "blocked"},
		"verifying":  {"publishing", "failed", "timed_out", "cancelled", "blocked"},
		"publishing": {"succeeded", "failed", "timed_out", "cancelled", "blocked"},
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
	for _, target := range []string{"succeeded", "failed", "timed_out", "cancelled", "blocked"} {
		t.Run(target, func(t *testing.T) {
			from := "running"
			if target == "succeeded" {
				from = "publishing"
			}
			f, token, id, _ := resultFixture(t, from)
			before := readResultSnapshot(t, f, id)
			input := resultBody(from, target)
			input["harnessHeadSha"] = strings.Repeat("b", 40)
			input["prUrl"] = "https://github.com/example/repo/pull/12"
			input["prNumber"] = 12
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
			wantRequests, wantBlocks, wantEvents, wantWorktree := 1, 0, 1, "retained"
			if target == "cancelled" {
				wantRequests = 0 // Spec 12 explicitly excludes cancellation.
			}
			if target == "blocked" {
				wantBlocks, wantEvents = 1, 2
			}
			if target == "succeeded" {
				wantWorktree = "active"
			}
			if after.Execution != target || after.Task != string(wantTask) || after.Worktree != wantWorktree ||
				after.Requests != wantRequests || after.Blocks != wantBlocks || after.Events != before.Events+wantEvents || after.Audits != 1 ||
				after.Finished.Valid != (target != "blocked") {
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
			var head, url string
			var number int
			if err := f.db.QueryRow(`SELECT harness_head_sha, pr_url, pr_number FROM executions WHERE id = ?`, id).Scan(&head, &url, &number); err != nil ||
				head != strings.Repeat("b", 40) || url != input["prUrl"] || number != 12 {
				t.Fatalf("saved metadata: %s %s %d err=%v", head, url, number, err)
			}
			if target == "blocked" {
				var cause, sha, reason, decision, eventBlock string
				err := f.db.QueryRow(`SELECT b.cause, b.head_sha, json_extract(b.detail, '$.reason'), coalesce(b.decision, ''),
					json_extract(e.payload, '$.blockId') FROM execution_blocks b JOIN execution_events e
					ON e.execution_id = b.execution_id AND e.type = 'blocked' AND json_extract(e.payload, '$.blockId') = b.id
					WHERE b.execution_id = ?`, id).Scan(&cause, &sha, &reason, &decision, &eventBlock)
				if err != nil || cause != "git_state" || sha != strings.Repeat("b", 40) || reason != "dirty" || decision != "" || eventBlock == "" {
					t.Fatalf("saved block: %s %s %s %s %s err=%v", cause, sha, reason, decision, eventBlock, err)
				}
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
	f, token, id, _ := resultFixture(t, "running")
	before := readResultSnapshot(t, f, id)
	for i, trigger := range []string{
		`BEFORE UPDATE OF harness_head_sha ON executions`,
		`BEFORE INSERT ON execution_blocks`,
		`BEFORE INSERT ON execution_events WHEN NEW.type = 'blocked'`,
		`BEFORE UPDATE OF status ON tasks`,
		`BEFORE UPDATE OF status ON worktrees`,
		`BEFORE INSERT ON notification_requests`,
		`BEFORE INSERT ON execution_events WHEN json_extract(NEW.payload, '$.reason') = 'runner_result'`,
		`BEFORE INSERT ON audit_logs WHEN NEW.action = 'execution.result'`,
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, err := f.db.Exec(`CREATE TRIGGER reject_result ` + trigger + ` BEGIN SELECT RAISE(ABORT, 'result storage unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			input := resultBody("running", "blocked")
			input["harnessHeadSha"] = strings.Repeat("c", 40)
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 500, Internal)
			if after := readResultSnapshot(t, f, id); after != before {
				t.Fatalf("partial result survived failure: before=%+v after=%+v", before, after)
			}
			var head sql.NullString
			if err := f.db.QueryRow(`SELECT harness_head_sha FROM executions WHERE id = ?`, id).Scan(&head); err != nil || head.Valid {
				t.Fatalf("metadata survived failure: %+v err=%v", head, err)
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
	for _, to := range []string{"preparing", "running", "verifying", "publishing"} {
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

func TestResultRejectsChangingSavedBaseSHA(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		for _, target := range []string{"running", "failed", "blocked"} {
			t.Run(fmt.Sprintf("context_pinned=%t/to_%s", pinned, target), func(t *testing.T) {
				f, token, input := contextFixture(t, ContextConfig{})
				attachContextWorktree(t, f)
				if pinned {
					status, body := postContext(t, f, token, contextPath, input)
					requireContext(t, status, body)
				}
				before := readResultSnapshot(t, f, otherExecution)
				revisions := pinnedRows(t, f)
				if pinned {
					var projects int
					for _, revision := range revisions {
						if revision.Layer == "project" {
							projects++
							if revision.SourceCommitSHA != input.HeadSHA {
								t.Fatalf("unexpected Project Context source: %+v", revision)
							}
						}
					}
					if projects == 0 {
						t.Fatal("regression requires pinned Project Context")
					}
				}
				report := resultBody("preparing", target)
				report["baseSha"] = strings.Repeat("c", 40)
				status, body := postResult(t, f, token, otherExecution, report)
				if status != 422 {
					t.Fatalf("base SHA mismatch report=%d %s; want 422", status, body)
				}
				assertAuthError(t, status, body, 422, PreconditionFailed)
				if after := readResultSnapshot(t, f, otherExecution); after != before {
					t.Fatalf("base SHA mismatch mutated DB: before=%+v after=%+v", before, after)
				}
				var base string
				if err := f.db.QueryRow(`SELECT base_sha FROM executions WHERE id = ?`, otherExecution).Scan(&base); err != nil || base != input.HeadSHA {
					t.Fatalf("saved base SHA changed: %s err=%v", base, err)
				}
				if after := pinnedRows(t, f); !reflect.DeepEqual(after, revisions) {
					t.Fatalf("base SHA mismatch mutated Context: before=%+v after=%+v", revisions, after)
				}
			})
		}
	}
}

func TestResultAcceptsInitialBaseSHA(t *testing.T) {
	f, token, id, _ := resultFixture(t, "claimed")
	var base sql.NullString
	if err := f.db.QueryRow(`SELECT base_sha FROM executions WHERE id = ?`, id).Scan(&base); err != nil || base.Valid {
		t.Fatalf("expected unset base SHA: %+v err=%v", base, err)
	}
	input := resultBody("claimed", "preparing")
	input["baseSha"] = strings.Repeat("a", 40)
	status, body := postResult(t, f, token, id, input)
	if status != 204 {
		t.Fatalf("initial base SHA report=%d %s", status, body)
	}
	if err := f.db.QueryRow(`SELECT base_sha FROM executions WHERE id = ?`, id).Scan(&base); err != nil || !base.Valid || base.String != input["baseSha"] {
		t.Fatalf("initial base SHA not saved: %+v err=%v", base, err)
	}
	if after := readResultSnapshot(t, f, id); after.Execution != "preparing" || after.Task != "running" || after.Requests != 0 {
		t.Fatalf("initial base SHA report state=%+v", after)
	}
}

func TestResultPreservesContextWithSameOrOmittedBaseSHA(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(fmt.Sprintf("same=%t", same), func(t *testing.T) {
			f, token, input := contextFixture(t, ContextConfig{})
			attachContextWorktree(t, f)
			status, body := postContext(t, f, token, contextPath, input)
			requireContext(t, status, body)
			revisions := pinnedRows(t, f)
			report := resultBody("preparing", "running")
			if same {
				report["baseSha"] = input.HeadSHA
			}
			status, body = postResult(t, f, token, otherExecution, report)
			if status != 204 {
				t.Fatalf("same or omitted base SHA report=%d %s", status, body)
			}
			if after := readResultSnapshot(t, f, otherExecution); after.Execution != "running" || after.Task != "running" || after.Requests != 0 {
				t.Fatalf("same or omitted base SHA report state=%+v", after)
			}
			var base string
			if err := f.db.QueryRow(`SELECT base_sha FROM executions WHERE id = ?`, otherExecution).Scan(&base); err != nil || base != input.HeadSHA {
				t.Fatalf("saved base SHA changed: %s err=%v", base, err)
			}
			if after := pinnedRows(t, f); !reflect.DeepEqual(after, revisions) {
				t.Fatalf("accepted result mutated Context: before=%+v after=%+v", revisions, after)
			}
		})
	}
}

func TestResultValidationLeavesStateAndNotificationRequestsUntouched(t *testing.T) {
	f, token, id, worktree := resultFixture(t, "running")
	before := readResultSnapshot(t, f, id)
	for _, tc := range []struct {
		name   string
		input  map[string]any
		status int
	}{
		{"missing block", map[string]any{"status": "blocked"}, 400},
		{"invalid cause", map[string]any{"status": "blocked", "block": map[string]any{"cause": "unknown", "headSha": strings.Repeat("a", 40)}}, 400},
		{"invalid head", map[string]any{"status": "blocked", "block": map[string]any{"cause": "git_state", "headSha": "invalid"}}, 400},
		{"missing class", map[string]any{"status": "failed"}, 400},
		{"unknown class", map[string]any{"status": "failed", "errorClass": "unknown"}, 400},
		{"wrong timeout class", map[string]any{"status": "timed_out", "errorClass": "cancelled"}, 400},
		{"block on progress", map[string]any{"status": "verifying", "block": map[string]any{"cause": "git_state", "headSha": strings.Repeat("a", 40)}}, 400},
		{"bad SHA", map[string]any{"status": "verifying", "verifiedSha": "oops"}, 400},
		{"unverified push", map[string]any{"status": "verifying", "pushedSha": strings.Repeat("a", 40)}, 422},
		{"wrong push", map[string]any{"status": "verifying", "verifiedSha": strings.Repeat("b", 40), "pushedSha": strings.Repeat("a", 40)}, 422},
		{"negative PR", map[string]any{"status": "verifying", "prNumber": -1}, 400},
		{"missing worktree", map[string]any{"status": "verifying", "worktreeId": ulid.Make().String()}, 422},
		{"missing gate", map[string]any{"status": "blocked", "block": map[string]any{"cause": "gate3_mismatch", "headSha": strings.Repeat("a", 40), "gateResultId": ulid.Make().String()}}, 422},
		{"unknown field", map[string]any{"status": "verifying", "runnerId": "claim-runner"}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.input["fromStatus"] = "running"
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
	// A real worktree owned by another Runner must also fail, not merely a
	// nonexistent foreign key. The same rule applies to another Task's tree.
	for _, change := range []string{"runner_id = 'mac-mini'", "status = 'cleaned'"} {
		if _, err := f.db.Exec(`UPDATE worktrees SET `+change+` WHERE id = ?`, worktree); err != nil {
			t.Fatal(err)
		}
		status, body := postResult(t, f, token, id, map[string]any{"fromStatus": "running", "status": "verifying", "worktreeId": worktree})
		assertAuthError(t, status, body, 422, PreconditionFailed)
	}
}

func TestResultRejectsHistoricalExecutionAndInconsistentTask(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(fmt.Sprint(historical), func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "running")
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
			status, body := postResult(t, f, token, id, resultBody("running", "blocked"))
			assertAuthError(t, status, body, 409, Conflict)
			if after := readResultSnapshot(t, f, id); after != before {
				t.Fatalf("stale execution/task changed: %+v", after)
			}
		})
	}
}

func TestResultAfterOwnerContinuationDoesNotRequestSameStateTwice(t *testing.T) {
	f, token, id, _ := resultFixture(t, "verifying")
	input := resultBody("verifying", "blocked")
	input["block"] = map[string]any{"cause": "gate1_test_change", "headSha": strings.Repeat("b", 40)}
	status, body := postResult(t, f, token, id, input)
	if status != 204 {
		t.Fatalf("first block=%d %s", status, body)
	}
	// Model the owner-only resume transaction, preserving the Execution ID.
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE execution_blocks SET decision = 'continue', decided_at = '2026-10-27T00:00:00Z' WHERE execution_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE executions SET resume_point = 'gate2' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := task.TransitionExecutionTx(context.Background(), tx, task.Transition{
		TaskID: "task-001", ExecutionID: id, FromTask: "blocked", FromExecution: "blocked", ToExecution: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	status, body = requestClaim(t, f, token)
	if resumed := requireClaim(t, status, body); resumed.Execution.ID != id {
		t.Fatalf("reclaimed another execution: %+v", resumed)
	}
	status, body = postResult(t, f, token, id, resultBody("claimed", "verifying"))
	if status != 204 {
		t.Fatalf("resume=%d %s", status, body)
	}
	status, body = postResult(t, f, token, id, input)
	if status != 204 {
		t.Fatalf("second block=%d %s", status, body)
	}
	if after := readResultSnapshot(t, f, id); after.Requests != 1 || after.Blocks != 2 || after.Execution != "blocked" || after.Task != "blocked" {
		t.Fatalf("repeated blocked state=%+v", after)
	}
}

func TestContextBuilderAndInterruptionShareNotificationRequests(t *testing.T) {
	for _, target := range []string{"blocked", "failed", "interrupted"} {
		t.Run(target, func(t *testing.T) {
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
			if target == "interrupted" {
				err = task.TransitionExecutionTx(context.Background(), tx, task.Transition{
					TaskID: execution.TaskID, ExecutionID: id, FromTask: execution.TaskStatus,
					FromExecution: execution.Status, ToExecution: model.ExecutionInterrupted,
				})
			} else {
				err = recordContextBuildError(context.Background(), tx, execution, strings.Repeat("b", 40),
					&contextBuildError{field: "profile", reason: "pin_mismatch", conflict: target == "blocked"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if after := readResultSnapshot(t, f, id); after.Requests != 1 || after.Execution != target || after.Worktree != "retained" {
				t.Fatalf("shared transition effects=%+v", after)
			}
		})
	}
}
