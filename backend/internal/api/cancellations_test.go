package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
)

type cancellationSnapshot struct {
	Execution, Task, ErrorClass   string
	Requested, Finished           sql.NullString
	Events, Audits, Notifications int
}

func cancellationState(t *testing.T, f *authFixture, id string) cancellationSnapshot {
	t.Helper()
	var s cancellationSnapshot
	err := f.db.QueryRow(`SELECT e.status, t.status, coalesce(e.error_class, ''),
		e.cancel_requested_at, e.finished_at,
		(SELECT count(*) FROM execution_events WHERE execution_id = e.id),
		(SELECT count(*) FROM audit_logs WHERE action = 'task.cancel' AND target_id = t.id),
		(SELECT count(*) FROM notification_requests WHERE execution_id = e.id)
		FROM executions e JOIN tasks t ON t.id = e.task_id
		WHERE e.id = ?`, id).Scan(&s.Execution, &s.Task, &s.ErrorClass,
		&s.Requested, &s.Finished, &s.Events, &s.Audits, &s.Notifications)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func executionTaskID(t *testing.T, f *authFixture, id string) string {
	t.Helper()
	var taskID string
	if err := f.db.QueryRow(`SELECT task_id FROM executions WHERE id = ?`, id).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	return taskID
}

func requestCancellation(t *testing.T, f *authFixture, taskID string) (int, []byte) {
	t.Helper()
	return f.request(t, "POST", "/api/tasks/"+taskID+"/cancel", true, f.csrf, nil)
}

func requireCancelled(t *testing.T, f *authFixture, id string, before cancellationSnapshot) {
	t.Helper()
	after := cancellationState(t, f, id)
	if after.Execution != "cancelled" || after.Task != "cancelled" || after.ErrorClass != "cancelled" ||
		after.Requested != before.Requested || !after.Finished.Valid || after.Events != before.Events+1 ||
		after.Audits != before.Audits+1 || after.Notifications != before.Notifications {
		t.Fatalf("cancellation before=%+v after=%+v", before, after)
	}
	if _, err := ParseTimestamp(after.Finished.String); err != nil {
		t.Fatal(err)
	}
	var from, to, reason string
	if err := f.db.QueryRow(`SELECT json_extract(payload, '$.from'), json_extract(payload, '$.to'),
		json_extract(payload, '$.reason') FROM execution_events WHERE execution_id = ? ORDER BY id DESC LIMIT 1`, id).
		Scan(&from, &to, &reason); err != nil || from != before.Execution || to != "cancelled" || reason != "task_cancelled" {
		t.Fatalf("cancel event: %s -> %s reason=%s err=%v", from, to, reason, err)
	}
	assertCancellationAudit(t, f, id, before.Task, "cancelled", false)
}

func assertCancellationAudit(t *testing.T, f *authFixture, id, from, to string, requested bool) {
	t.Helper()
	var actor, targetType, targetID, detail string
	if err := f.db.QueryRow(`SELECT actor, target_type, target_id, detail FROM audit_logs
		WHERE action = 'task.cancel' AND json_extract(detail, '$.executionId') = ? ORDER BY id DESC LIMIT 1`, id).
		Scan(&actor, &targetType, &targetID, &detail); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(detail), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"executionId": id, "from": from, "to": to, "cancelRequested": requested}
	if actor != "owner" || targetType != "task" || targetID != executionTaskID(t, f, id) || !reflect.DeepEqual(got, want) {
		t.Fatalf("cancel audit: %s %s %s %s", actor, targetType, targetID, detail)
	}
}

func TestCancelQueuedImmediatelyAndNeverClaimed(t *testing.T) {
	f, token, _ := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "cancel-queued")
	var id string
	if err := f.db.QueryRow(`SELECT id FROM executions WHERE task_id = ?`, created.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	before := cancellationState(t, f, id)
	status, body := requestCancellation(t, f, created.ID)
	if status != 204 || len(body) != 0 {
		t.Fatalf("cancel=%d %s", status, body)
	}
	requireCancelled(t, f, id, before)
	status, body = requestClaim(t, f, token)
	requireNoJob(t, status, body)
	after := cancellationState(t, f, id)
	status, body = requestCancellation(t, f, created.ID)
	assertAuthError(t, status, body, 409, Conflict)
	if got := cancellationState(t, f, id); got != after {
		t.Fatalf("repeated terminal cancel mutated DB: %+v", got)
	}
}

func TestCancelBlocked(t *testing.T) {
	f, _, id, _ := resultFixture(t, "checking")
	blockExecution(t, f, id, "checking")
	before := cancellationState(t, f, id)
	status, body := requestCancellation(t, f, executionTaskID(t, f, id))
	if status != 204 {
		t.Fatalf("cancel blocked=%d %s", status, body)
	}
	requireCancelled(t, f, id, before)
}

func postHeartbeat(t *testing.T, f *authFixture, token, runner string, input any) (int, []byte) {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return f.request(t, "POST", "/api/runners/"+runner+"/heartbeat", false, "", func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Body = io.NopCloser(strings.NewReader(string(data)))
	})
}

func heartbeatInput(ids ...string) map[string]any {
	if ids == nil {
		ids = []string{}
	}
	return map[string]any{"bootId": "test-boot", "runningExecutionIds": ids}
}

func requireHeartbeat(t *testing.T, status int, body []byte, ids ...string) {
	t.Helper()
	if ids == nil {
		ids = []string{}
	}
	var response struct {
		IDs []string `json:"cancelExecutionIds"`
	}
	if status != 200 || json.Unmarshal(body, &response) != nil || !reflect.DeepEqual(response.IDs, ids) {
		t.Fatalf("heartbeat=%d %s; want %v", status, body, ids)
	}
}

func TestCancelActiveRequestsUntilRunnerReportsCancellation(t *testing.T) {
	for _, from := range []string{"claimed", "preparing", "working", "checking", "publishing"} {
		t.Run(from, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, from)
			before := cancellationState(t, f, id)
			status, body := postHeartbeat(t, f, token, "claim-runner", heartbeatInput(id))
			requireHeartbeat(t, status, body)
			status, body = requestCancellation(t, f, executionTaskID(t, f, id))
			if status != 204 {
				t.Fatalf("cancel=%d %s", status, body)
			}
			after := cancellationState(t, f, id)
			if !after.Requested.Valid {
				t.Fatal("cancel_requested_at missing")
			}
			if _, err := ParseTimestamp(after.Requested.String); err != nil {
				t.Fatal(err)
			}
			want := before
			want.Requested = after.Requested
			want.Audits++
			if after != want {
				t.Fatalf("request changed state: got=%+v want=%+v", after, want)
			}
			assertCancellationAudit(t, f, id, before.Task, before.Task, true)
			status, body = requestCancellation(t, f, executionTaskID(t, f, id))
			if status != 204 || cancellationState(t, f, id) != after {
				t.Fatalf("request retry changed first timestamp/audit: %d %s", status, body)
			}
			for i := 0; i < 2; i++ {
				status, body = postHeartbeat(t, f, token, "claim-runner", heartbeatInput(id))
				requireHeartbeat(t, status, body, id)
				if got := cancellationState(t, f, id); got != after {
					t.Fatalf("heartbeat changed request/state: %+v", got)
				}
			}
			var runnerStatus, runnerAt, executionAt string
			if err := f.db.QueryRow(`SELECT r.status, coalesce(r.last_heartbeat_at, ''), e.last_heartbeat_at
				FROM executions e JOIN runners r ON r.id = e.runner_id WHERE e.id = ?`, id).
				Scan(&runnerStatus, &runnerAt, &executionAt); err != nil || runnerStatus != "online" || runnerAt != executionAt {
				t.Fatalf("liveness: %s %s %s err=%v", runnerStatus, runnerAt, executionAt, err)
			}
			if _, err := ParseTimestamp(runnerAt); err != nil {
				t.Fatal(err)
			}
			status, body = postResult(t, f, token, id, resultBody(from, "cancelled"))
			if status != 204 {
				t.Fatalf("runner cancelled=%d %s", status, body)
			}
			finished := cancellationState(t, f, id)
			if finished.Execution != "cancelled" || finished.Task != "cancelled" || finished.ErrorClass != "cancelled" ||
				!finished.Finished.Valid || finished.Requested != after.Requested ||
				finished.Events <= after.Events || finished.Audits != after.Audits || finished.Notifications != 0 {
				t.Fatalf("runner cancellation=%+v", finished)
			}
			status, body = postHeartbeat(t, f, token, "claim-runner", heartbeatInput(id))
			requireHeartbeat(t, status, body)
		})
	}
}

func TestCancelCompletedAndOtherForbiddenStatesDoNotWrite(t *testing.T) {
	for _, state := range []struct{ execution, task string }{
		{"succeeded", "completed"}, {"failed", "failed"}, {"timed_out", "timed_out"},
		{"interrupted", "failed"}, {"cancelled", "cancelled"}, {"pending", "received"},
		{"pending", "routing"}, {"pending", "waiting_approval"}, {"working", "queued"},
	} {
		t.Run(state.task+"_"+state.execution, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "publishing")
			if state.task == "completed" {
				status, body := postResult(t, f, token, id, resultBody("publishing", "succeeded"))
				if status != 204 {
					t.Fatalf("complete=%d %s", status, body)
				}
			} else {
				if _, err := f.db.Exec(`UPDATE executions SET status = ? WHERE id = ?`, state.execution, id); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE tasks SET status = ? WHERE id = (SELECT task_id FROM executions WHERE id = ?)`, state.task, id); err != nil {
					t.Fatal(err)
				}
			}
			before := cancellationState(t, f, id)
			status, body := requestCancellation(t, f, executionTaskID(t, f, id))
			assertAuthError(t, status, body, 409, Conflict)
			if after := cancellationState(t, f, id); after != before {
				t.Fatalf("rejection mutated DB: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestCancelOnlyLatestExecutionAndValidTask(t *testing.T) {
	f, _, id, _ := resultFixture(t, "working")
	newID := ulid.Make().String()
	if _, err := f.db.Exec(`UPDATE executions SET status = 'failed' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		SELECT ?, task_id, seq+1, 'continue', 'pending', created_at FROM executions WHERE id = ?`, newID, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE tasks SET status = 'queued' WHERE id = (SELECT task_id FROM executions WHERE id = ?)`, id); err != nil {
		t.Fatal(err)
	}
	before := cancellationState(t, f, newID)
	status, body := requestCancellation(t, f, executionTaskID(t, f, id))
	if status != 204 {
		t.Fatalf("latest cancel=%d %s", status, body)
	}
	requireCancelled(t, f, newID, before)
	old := cancellationState(t, f, id)
	if old.Execution != "failed" || old.Requested.Valid || old.Finished.Valid {
		t.Fatalf("historical execution modified: %+v", old)
	}
	status, body = requestCancellation(t, f, "task-999999")
	assertAuthError(t, status, body, 404, NotFound)
	status, body = requestCancellation(t, f, "invalid")
	assertAuthError(t, status, body, 400, ValidationFailed)
}

func TestCancellationRequiresOwnerSessionCSRFAndBoundary(t *testing.T) {
	f, token, id, _ := resultFixture(t, "working")
	path := "/api/tasks/" + executionTaskID(t, f, id) + "/cancel"
	for _, tc := range []struct {
		name, reason string
		status       int
	}{
		{"session", "invalid_session", 401}, {"csrf", "invalid_csrf", 403},
		{"origin", "origin_mismatch", 403}, {"missing origin", "origin_mismatch", 403},
		{"host", "host_mismatch", 403}, {"runner", "wrong_auth_class", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := cancellationState(t, f, id)
			csrf := f.csrf
			if tc.name == "csrf" {
				csrf = ""
			}
			status, body := f.request(t, "POST", path, tc.name != "session" && tc.name != "runner", csrf, func(r *http.Request) {
				switch tc.name {
				case "origin":
					r.Header.Set("Origin", "http://evil.example")
				case "missing origin":
					r.Header.Del("Origin")
				case "host":
					r.Host = "evil.example"
				case "runner":
					r.Header.Set("Authorization", "Bearer "+token)
				}
			})
			code := Forbidden
			if tc.status == 401 {
				code = Unauthenticated
			}
			assertAuthError(t, status, body, tc.status, code)
			var count int
			if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action = 'auth.reject' AND target_id = ?
				AND json_extract(detail, '$.reason') = ?`, path, tc.reason).Scan(&count); err != nil || count < 1 {
				t.Fatalf("auth audit missing reason=%s count=%d err=%v", tc.reason, count, err)
			}
			if after := cancellationState(t, f, id); after != before {
				t.Fatalf("auth denial changed state: %+v", after)
			}
		})
	}
}

func TestCancelRollsBackRequestTransitionAndRecordsOnFailure(t *testing.T) {
	for _, from := range []string{"claimed", "blocked"} {
		f, _, id, _ := resultFixture(t, "claimed")
		if from == "blocked" {
			// Spec 02 5.2: a claimed Execution cannot block before a stage.
			f, _, id, _ = resultFixture(t, "working")
			blockExecution(t, f, id, "working")
		}
		triggers := []string{`BEFORE INSERT ON audit_logs WHEN NEW.action = 'task.cancel'`}
		if from == "claimed" {
			triggers = append(triggers, `BEFORE UPDATE OF cancel_requested_at ON executions`)
		} else {
			triggers = append(triggers, `BEFORE UPDATE OF error_class ON executions`, `BEFORE UPDATE OF status ON tasks`,
				`BEFORE UPDATE OF finished_at ON executions`, `BEFORE INSERT ON execution_events`)
		}
		for i, trigger := range triggers {
			t.Run(fmt.Sprintf("%s/%d", from, i), func(t *testing.T) {
				before := cancellationState(t, f, id)
				if _, err := f.db.Exec(`CREATE TRIGGER reject_cancel ` + trigger + ` BEGIN SELECT RAISE(ABORT, 'cancel unavailable'); END`); err != nil {
					t.Fatal(err)
				}
				status, body := requestCancellation(t, f, executionTaskID(t, f, id))
				assertAuthError(t, status, body, 500, Internal)
				if after := cancellationState(t, f, id); after != before {
					t.Fatalf("partial cancellation survived: before=%+v after=%+v", before, after)
				}
				if _, err := f.db.Exec(`DROP TRIGGER reject_cancel`); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestClaimAndCancelSerializeWithoutLosingCancellation(t *testing.T) {
	for i := 0; i < 5; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f, token, _ := claimFixture(t)
			created := createClaimTask(t, f, "project-a", "claim-cancel-race")
			var id string
			if err := f.db.QueryRow(`SELECT id FROM executions WHERE task_id = ?`, created.ID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				status, body := requestClaim(t, f, token)
				if status != 200 {
					t.Errorf("claim=%d %s", status, body)
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				status, body := requestCancellation(t, f, created.ID)
				if status != 204 {
					t.Errorf("cancel=%d %s", status, body)
				}
			}()
			close(start)
			wg.Wait()
			s := cancellationState(t, f, id)
			if s.Execution == "cancelled" {
				if s.Task != "cancelled" || s.Requested.Valid || s.Events != 2 || !s.Finished.Valid {
					t.Fatalf("cancel first=%+v", s)
				}
			} else if s.Execution == "claimed" {
				if s.Task != "running" || !s.Requested.Valid || s.Events != 2 || s.Finished.Valid {
					t.Fatalf("claim first=%+v", s)
				}
				status, body := postHeartbeat(t, f, token, "claim-runner", heartbeatInput(id))
				requireHeartbeat(t, status, body, id)
			} else {
				t.Fatalf("unexpected race state=%+v", s)
			}
			if s.Audits != 1 || s.Notifications != 0 {
				t.Fatalf("race effects=%+v", s)
			}
		})
	}
}

func TestHeartbeatNeverLeaksOrRenewsAnotherRunnersExecutions(t *testing.T) {
	f, token, id, _ := resultFixture(t, "working")
	status, body := requestCancellation(t, f, executionTaskID(t, f, id))
	if status != 204 {
		t.Fatalf("cancel=%d %s", status, body)
	}
	otherToken, err := runnerauth.Register(context.Background(), f.db, "other-runner", "Other", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Even if Project ownership changes, the Execution belongs to its claimant.
	if _, err := f.db.Exec(`UPDATE projects SET runner_id = 'other-runner' WHERE id = 'project-a'`); err != nil {
		t.Fatal(err)
	}
	status, body = postHeartbeat(t, f, otherToken, "other-runner", heartbeatInput())
	requireHeartbeat(t, status, body)
	for _, foreignID := range []string{id, ulid.Make().String()} {
		status, body = postHeartbeat(t, f, otherToken, "other-runner", heartbeatInput(foreignID))
		assertAuthError(t, status, body, 403, Forbidden)
		var denials int
		if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action = 'auth.reject'
			AND json_extract(detail, '$.reason') = 'execution_runner_mismatch'`).Scan(&denials); err != nil || denials < 1 {
			t.Fatalf("ownership denial audit=%d err=%v", denials, err)
		}
	}
	var heartbeat sql.NullString
	if err := f.db.QueryRow(`SELECT last_heartbeat_at FROM executions WHERE id = ?`, id).Scan(&heartbeat); err != nil || heartbeat.Valid {
		t.Fatalf("foreign heartbeat renewed execution=%+v err=%v", heartbeat, err)
	}
	status, body = postHeartbeat(t, f, token, "claim-runner", heartbeatInput())
	requireHeartbeat(t, status, body, id)
	status, body = postHeartbeat(t, f, otherToken, "claim-runner", heartbeatInput())
	assertAuthError(t, status, body, 403, Forbidden)
	status, body = postHeartbeat(t, f, "invalid", "claim-runner", heartbeatInput())
	assertAuthError(t, status, body, 401, Unauthenticated)
}

func TestHeartbeatValidationAndStorageFailureAreAtomic(t *testing.T) {
	f, token, id, _ := resultFixture(t, "working")
	for _, input := range []any{
		map[string]any{"runningExecutionIds": []string{}},
		map[string]any{"bootId": "test"},
		map[string]any{"bootId": "test", "runningExecutionIds": []string{"invalid"}},
		map[string]any{"bootId": "test", "runningExecutionIds": []string{}, "unknown": true},
	} {
		status, body := postHeartbeat(t, f, token, "claim-runner", input)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_heartbeat BEFORE UPDATE OF last_heartbeat_at ON executions
		BEGIN SELECT RAISE(ABORT, 'heartbeat unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	status, body := postHeartbeat(t, f, token, "claim-runner", heartbeatInput(id))
	assertAuthError(t, status, body, 500, Internal)
	var runnerStatus, runnerAt string
	var executionAt sql.NullString
	if err := f.db.QueryRow(`SELECT r.status, coalesce(r.last_heartbeat_at, ''), e.last_heartbeat_at
		FROM executions e JOIN runners r ON r.id = e.runner_id WHERE e.id = ?`, id).
		Scan(&runnerStatus, &runnerAt, &executionAt); err != nil || runnerStatus != "offline" || runnerAt != "" || executionAt.Valid {
		t.Fatalf("partial heartbeat survived: %s %s %+v err=%v", runnerStatus, runnerAt, executionAt, err)
	}
}

// A Linear task held at intake has no Execution; the owner cancels it on the
// blocked path (spec 15 3.6.3, spec 04 3.5). A blocked task without an
// Execution that is not held is still a conflict.
func TestCancelHeldLinearTaskWithoutExecution(t *testing.T) {
	f, _, _ := claimFixture(t)
	for _, task := range []struct{ id, cause string }{{"task-900001", "missing_acceptance_criteria"}, {"task-900002", ""}} {
		if _, err := f.db.Exec(`INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at, source, intake_block_cause)
			VALUES (?, 'project-a', 't', 'b', 'ac', 'solo', 'blocked', ?, 't', 't', 'linear', nullif(?, ''))`,
			task.id, "linear:"+task.id+":1", task.cause); err != nil {
			t.Fatal(err)
		}
		// A Linear task is always created with its Issue link (spec 15 3.6);
		// the cancellation comment needs it (spec 16 3.7).
		if _, err := f.db.Exec(`INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url,
			team_id, task_id, attach_seq, created_at) VALUES (?, ?, 'ENG-1', 'https://linear.app/x/ENG-1', 'team-a', ?, 1, 't')`,
			"link-"+task.id, "issue-"+task.id, task.id); err != nil {
			t.Fatal(err)
		}
	}
	status, body := requestCancellation(t, f, "task-900001")
	if status != 204 {
		t.Fatalf("cancel held=%d %s", status, body)
	}
	var taskStatus, actor, detail string
	if err := f.db.QueryRow(`SELECT t.status, a.actor, a.detail FROM tasks t
		JOIN audit_logs a ON a.target_id = t.id AND a.action = 'task.cancel' WHERE t.id = 'task-900001'`).
		Scan(&taskStatus, &actor, &detail); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "cancelled" || actor != "owner" || detail != `{"cancelRequested":false,"from":"blocked","to":"cancelled"}` {
		t.Fatalf("held cancel: %s %s %s", taskStatus, actor, detail)
	}
	status, body = requestCancellation(t, f, "task-900002")
	assertAuthError(t, status, body, 409, Conflict)
	status, body = requestCancellation(t, f, "task-900001")
	assertAuthError(t, status, body, 409, Conflict)
}
