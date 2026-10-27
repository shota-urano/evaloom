package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

type recoverySnapshot struct {
	Execution, Task, Worktree                         string
	ErrorClass, Message, Finished, ExecutionHeartbeat sql.NullString
	Executions, Events, Audits, Notifications         int
}

func recoveryState(t *testing.T, f *authFixture, id string) recoverySnapshot {
	t.Helper()
	var s recoverySnapshot
	err := f.db.QueryRow(`SELECT e.status, t.status, coalesce(w.status, ''), e.error_class, e.error_message,
		e.finished_at, e.last_heartbeat_at,
		(SELECT count(*) FROM executions),
		(SELECT count(*) FROM execution_events WHERE execution_id = e.id),
		(SELECT count(*) FROM audit_logs WHERE action = 'execution.interrupt' AND target_id = e.id),
		(SELECT count(*) FROM notification_requests WHERE execution_id = e.id)
		FROM executions e JOIN tasks t ON t.id = e.task_id LEFT JOIN worktrees w ON w.id = e.worktree_id
		WHERE e.id = ?`, id).Scan(&s.Execution, &s.Task, &s.Worktree, &s.ErrorClass, &s.Message,
		&s.Finished, &s.ExecutionHeartbeat, &s.Executions, &s.Events, &s.Audits, &s.Notifications)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func requireInterrupted(t *testing.T, f *authFixture, id string, before recoverySnapshot, reason string) {
	t.Helper()
	after := recoveryState(t, f, id)
	want := before
	want.Execution, want.Task = "interrupted", "failed"
	if before.Worktree != "" {
		want.Worktree = "retained"
	}
	want.ErrorClass = sql.NullString{String: "interrupted", Valid: true}
	want.Message = sql.NullString{String: reason, Valid: true}
	want.Finished = after.Finished
	want.Events++
	want.Audits++
	want.Notifications++
	if after != want || !after.Finished.Valid {
		t.Fatalf("recovery got=%+v want=%+v", after, want)
	}
	if _, err := ParseTimestamp(after.Finished.String); err != nil {
		t.Fatal(err)
	}
	var payload, actor, targetType, detail, notification string
	if err := f.db.QueryRow(`SELECT payload FROM execution_events WHERE execution_id = ? ORDER BY id DESC LIMIT 1`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event map[string]string
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(event, map[string]string{"from": before.Execution, "to": "interrupted", "reason": reason}) {
		t.Fatalf("event=%s", payload)
	}
	if err := f.db.QueryRow(`SELECT actor, target_type, detail FROM audit_logs WHERE action = 'execution.interrupt' AND target_id = ?`, id).Scan(&actor, &targetType, &detail); err != nil {
		t.Fatal(err)
	}
	var audit map[string]string
	if err := json.Unmarshal([]byte(detail), &audit); err != nil {
		t.Fatal(err)
	}
	if actor != "system" || targetType != "execution" || !reflect.DeepEqual(audit, map[string]string{
		"taskId": executionTaskID(t, f, id), "runnerId": "claim-runner", "from": before.Execution, "to": "interrupted", "reason": reason,
	}) {
		t.Fatalf("audit=%s %s %s", actor, targetType, detail)
	}
	if err := f.db.QueryRow(`SELECT status FROM notification_requests WHERE execution_id = ?`, id).Scan(&notification); err != nil || notification != "interrupted" {
		t.Fatalf("notification=%s err=%v", notification, err)
	}
}

func TestRecoveryAllOwnedExecutionsAndRecordsAreAtomic(t *testing.T) {
	for _, path := range []string{"boot", "startup"} {
		t.Run(path, func(t *testing.T) {
			f, token, first, _ := resultFixture(t, "running")
			createClaimTask(t, f, "project-b", "second-recovery")
			status, body := requestClaim(t, f, token)
			second := requireClaim(t, status, body).Execution.ID // No worktree yet.
			createClaimTask(t, f, "project-b", "keep-pending")
			status, body = postHeartbeat(t, f, token, "claim-runner", heartbeatInput())
			requireHeartbeat(t, status, body)
			beforeFirst, beforeSecond := recoveryState(t, f, first), recoveryState(t, f, second)
			otherToken, err := runnerauth.Register(context.Background(), f.db, "other-runner", "Other", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			input := heartbeatInput(first)
			input["bootId"] = "changed"
			status, body = postHeartbeat(t, f, otherToken, "other-runner", input)
			assertAuthError(t, status, body, 403, Forbidden)
			var boot sql.NullString
			if err := f.db.QueryRow(`SELECT boot_id FROM runners WHERE id = 'other-runner'`).Scan(&boot); err != nil || boot.Valid {
				t.Fatalf("denied heartbeat saved boot: %+v %v", boot, err)
			}
			if _, err := f.db.Exec(fmt.Sprintf(`CREATE TRIGGER reject_second BEFORE INSERT ON notification_requests
				WHEN NEW.execution_id = '%s' BEGIN SELECT RAISE(ABORT, 'second recovery unavailable'); END`, second)); err != nil {
				t.Fatal(err)
			}
			recover := func(fail bool) {
				if path == "boot" {
					input := heartbeatInput()
					input["bootId"] = "new-boot"
					status, body := postHeartbeat(t, f, token, "claim-runner", input)
					if fail {
						assertAuthError(t, status, body, 500, Internal)
					} else {
						requireHeartbeat(t, status, body)
					}
				} else {
					started := time.Now().UTC().Add(time.Minute)
					err := store.RecoverStaleRunners(context.Background(), f.db, started, started.Add(store.StartupHeartbeatGrace))
					if fail && err == nil || !fail && err != nil {
						t.Fatalf("startup failure=%v err=%v", fail, err)
					}
				}
			}
			recover(true)
			if recoveryState(t, f, first) != beforeFirst || recoveryState(t, f, second) != beforeSecond {
				t.Fatal("partial multi-Execution recovery survived")
			}
			if _, err := f.db.Exec(`DROP TRIGGER reject_second`); err != nil {
				t.Fatal(err)
			}
			recover(false)
			reason := "runner_boot_changed"
			if path == "startup" {
				reason = "runner_heartbeat_lost"
			}
			requireInterrupted(t, f, first, beforeFirst, reason)
			requireInterrupted(t, f, second, beforeSecond, reason)
			var pending int
			if err := f.db.QueryRow(`SELECT count(*) FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.status = 'pending' AND t.status = 'queued'`).Scan(&pending); err != nil || pending != 1 {
				t.Fatalf("pending execution changed: %d err=%v", pending, err)
			}
		})
	}
}

func TestRecoveryBothPathsInterruptEveryActiveState(t *testing.T) {
	for _, path := range []string{"boot", "startup"} {
		for _, from := range []string{"claimed", "preparing", "running", "verifying", "publishing"} {
			t.Run(path+"/"+from, func(t *testing.T) {
				f, token, id, wt := resultFixture(t, from)
				// Initial heartbeat establishes the baseline without interrupting.
				before := recoveryState(t, f, id)
				status, body := postHeartbeat(t, f, token, "claim-runner", heartbeatInput())
				requireHeartbeat(t, status, body)
				if got := recoveryState(t, f, id); got != before {
					t.Fatalf("initial boot interrupted: %+v", got)
				}
				file := filepath.Join(t.TempDir(), "uncommitted.txt")
				if err := os.WriteFile(file, []byte("unfinished changes"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE worktrees SET path = ? WHERE id = ?`, filepath.Dir(file), wt); err != nil {
					t.Fatal(err)
				}
				reason := "runner_boot_changed"
				if path == "boot" {
					input := heartbeatInput() // New process reports no running IDs.
					input["bootId"] = "next-boot"
					status, body = postHeartbeat(t, f, token, "claim-runner", input)
					requireHeartbeat(t, status, body)
				} else {
					reason = "runner_heartbeat_lost"
					started := time.Now().UTC().Add(time.Minute)
					if err := store.RecoverStaleRunners(context.Background(), f.db, started, started.Add(store.StartupHeartbeatGrace)); err != nil {
						t.Fatal(err)
					}
				}
				requireInterrupted(t, f, id, before, reason)
				if content, err := os.ReadFile(file); err != nil || string(content) != "unfinished changes" {
					t.Fatalf("worktree contents=%q err=%v", content, err)
				}
				after := recoveryState(t, f, id)
				// Repeated recovery and late heartbeats/results cannot reopen it.
				started := time.Now().UTC().Add(time.Hour)
				if err := store.RecoverStaleRunners(context.Background(), f.db, started, started.Add(store.StartupHeartbeatGrace)); err != nil {
					t.Fatal(err)
				}
				status, body = postHeartbeat(t, f, token, "claim-runner", heartbeatInput(id))
				requireHeartbeat(t, status, body)
				status, body = postResult(t, f, token, id, resultBody(from, "failed"))
				assertAuthError(t, status, body, 409, Conflict)
				status, body = requestClaim(t, f, token)
				if status != 204 {
					t.Fatalf("recovery requeued work: %d %s", status, body)
				}
				if got := recoveryState(t, f, id); got != after {
					t.Fatalf("repeat mutated recovery: %+v", got)
				}
			})
		}
	}
}

func TestSameBootAndResumedHeartbeatKeepExecution(t *testing.T) {
	f, token, id, _ := resultFixture(t, "running")
	status, body := postHeartbeat(t, f, token, "claim-runner", heartbeatInput(id))
	requireHeartbeat(t, status, body)
	started := time.Now().UTC()
	before := recoveryState(t, f, id)
	// A long outage before startup must not bypass the reconnection grace.
	if _, err := f.db.Exec(`UPDATE runners SET last_heartbeat_at = '2020-01-01T00:00:00Z' WHERE id = 'claim-runner'`); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverStaleRunners(context.Background(), f.db, started, started.Add(store.StartupHeartbeatGrace-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if got := recoveryState(t, f, id); got != before {
		t.Fatalf("interrupted during grace: %+v", got)
	}
	status, body = postHeartbeat(t, f, token, "claim-runner", heartbeatInput(id))
	requireHeartbeat(t, status, body)
	resumed := recoveryState(t, f, id)
	want := before
	want.ExecutionHeartbeat = resumed.ExecutionHeartbeat
	if resumed != want || !resumed.ExecutionHeartbeat.Valid {
		t.Fatalf("same boot changed lifecycle: %+v", resumed)
	}
	if err := store.RecoverStaleRunners(context.Background(), f.db, started, started.Add(store.StartupHeartbeatGrace)); err != nil {
		t.Fatal(err)
	}
	if got := recoveryState(t, f, id); got != resumed {
		t.Fatalf("resumed runner interrupted: %+v", got)
	}
	status, body = postResult(t, f, token, id, resultBody("running", "verifying"))
	if status != 204 {
		t.Fatalf("could not continue: %d %s", status, body)
	}
}

func TestRecoveryOnlyTouchesActiveOwnedExecutions(t *testing.T) {
	f, token, id, _ := resultFixture(t, "running")
	other, err := runnerauth.Register(context.Background(), f.db, "other-runner", "Other", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	status, body := postHeartbeat(t, f, other, "other-runner", heartbeatInput())
	requireHeartbeat(t, status, body)
	before := recoveryState(t, f, id)
	input := heartbeatInput()
	input["bootId"] = "new"
	status, body = postHeartbeat(t, f, other, "other-runner", input)
	requireHeartbeat(t, status, body)
	if got := recoveryState(t, f, id); got != before {
		t.Fatalf("another runner interrupted owner: %+v", got)
	}
	// Project reassignment must not change ownership of the claimed Execution.
	if _, err := f.db.Exec(`UPDATE projects SET runner_id = 'other-runner' WHERE id = 'project-a'`); err != nil {
		t.Fatal(err)
	}
	status, body = postHeartbeat(t, f, token, "claim-runner", heartbeatInput())
	requireHeartbeat(t, status, body)
	status, body = postHeartbeat(t, f, token, "claim-runner", input)
	requireHeartbeat(t, status, body)
	requireInterrupted(t, f, id, before, "runner_boot_changed")
	for _, state := range []struct{ execution, task string }{
		{"pending", "queued"}, {"blocked", "blocked"}, {"failed", "failed"}, {"succeeded", "completed"}, {"cancelled", "cancelled"}, {"timed_out", "timed_out"}, {"interrupted", "failed"},
	} {
		t.Run(state.execution, func(t *testing.T) {
			if _, err := f.db.Exec(`UPDATE executions SET status = ? WHERE id = ?`, state.execution, id); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE tasks SET status = ? WHERE id = (SELECT task_id FROM executions WHERE id = ?)`, state.task, id); err != nil {
				t.Fatal(err)
			}
			before := recoveryState(t, f, id)
			input["bootId"] = state.execution
			status, body := postHeartbeat(t, f, token, "claim-runner", input)
			requireHeartbeat(t, status, body)
			started := time.Now().UTC().Add(time.Minute)
			if err := store.RecoverStaleRunners(context.Background(), f.db, started, started.Add(store.StartupHeartbeatGrace)); err != nil {
				t.Fatal(err)
			}
			if got := recoveryState(t, f, id); got != before {
				t.Fatalf("non-active changed: %+v", got)
			}
		})
	}
}

func TestRecoveryFailuresRollBackBootLivenessAndAllEffects(t *testing.T) {
	for _, path := range []string{"boot", "startup"} {
		for _, trigger := range []string{
			`BEFORE UPDATE OF error_class ON executions`, `BEFORE UPDATE OF status ON tasks`,
			`BEFORE UPDATE OF status ON worktrees`, `BEFORE UPDATE OF finished_at ON executions`,
			`BEFORE INSERT ON execution_events`, `BEFORE INSERT ON audit_logs`, `BEFORE INSERT ON notification_requests`,
		} {
			t.Run(path+"/"+trigger, func(t *testing.T) {
				f, token, id, _ := resultFixture(t, "running")
				status, body := postHeartbeat(t, f, token, "claim-runner", heartbeatInput())
				requireHeartbeat(t, status, body)
				before := recoveryState(t, f, id)
				var runnerBefore string
				if err := f.db.QueryRow(`SELECT json_array(boot_id, status, last_heartbeat_at) FROM runners WHERE id = 'claim-runner'`).Scan(&runnerBefore); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`CREATE TRIGGER reject_recovery ` + trigger + ` BEGIN SELECT RAISE(ABORT, 'recovery unavailable'); END`); err != nil {
					t.Fatal(err)
				}
				recover := func() error {
					if path == "startup" {
						started := time.Now().UTC().Add(time.Minute)
						return store.RecoverStaleRunners(context.Background(), f.db, started, started.Add(store.StartupHeartbeatGrace))
					}
					_, err := store.Heartbeat(context.Background(), f.db, "claim-runner", "next", nil)
					return err
				}
				if err := recover(); err == nil {
					t.Fatal("expected recovery failure")
				}
				if got := recoveryState(t, f, id); got != before {
					t.Fatalf("partial recovery: %+v", got)
				}
				var runnerAfter string
				if err := f.db.QueryRow(`SELECT json_array(boot_id, status, last_heartbeat_at) FROM runners WHERE id = 'claim-runner'`).Scan(&runnerAfter); err != nil || runnerAfter != runnerBefore {
					t.Fatalf("runner changed: %s err=%v", runnerAfter, err)
				}
				if _, err := f.db.Exec(`DROP TRIGGER reject_recovery`); err != nil {
					t.Fatal(err)
				}
				if err := recover(); err != nil {
					t.Fatal(err)
				}
				reason := "runner_boot_changed"
				if path == "startup" {
					reason = "runner_heartbeat_lost"
				}
				requireInterrupted(t, f, id, before, reason)
			})
		}
	}
}
