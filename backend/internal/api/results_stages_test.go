package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

func stageExec(t *testing.T, f *authFixture, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
func stageCount(t *testing.T, f *authFixture, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func stageOK(t *testing.T, f *authFixture, token, id string, input any) {
	t.Helper()
	status, body := postResult(t, f, token, id, input)
	if status != 204 {
		t.Fatalf("stage result: %d %s", status, body)
	}
}
func createdReport(t *testing.T) (*authFixture, string, string, map[string]any) {
	t.Helper()
	f, token, id, _ := resultFixture(t, "preparing")
	stageExec(t, f, `UPDATE executions SET worktree_id=NULL,base_sha=?,branch_name=NULL WHERE id=?`, strings.Repeat("a", 40), id)
	return f, token, id, map[string]any{"fromStatus": "preparing", "status": "preparing", "worktree": map[string]any{
		"action": "created", "id": ulid.Make().String(), "path": "/worktrees/new", "branchName": "evaloom/task-new", "baseSha": strings.Repeat("a", 40),
		"copiedFiles": []map[string]string{{"path": "dummy.config", "sha256": strings.Repeat("b", 64)}},
	}}
}
func stageEvent(t *testing.T, f *authFixture, id, kind string) map[string]any {
	t.Helper()
	var data string
	if err := f.db.QueryRow(`SELECT payload FROM execution_events WHERE execution_id=? AND type=? ORDER BY id DESC LIMIT 1`, id, kind).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestStageCreatedAtomicAndIdempotent(t *testing.T) {
	for _, command := range []string{"", "npm ci"} {
		t.Run(command, func(t *testing.T) {
			f, token, id, input := createdReport(t)
			stageExec(t, f, `UPDATE projects SET setup_command=? WHERE id='project-a'`, command)
			w := input["worktree"].(map[string]any)
			before := stageCount(t, f, `SELECT count(*) FROM worktrees`)
			stageOK(t, f, token, id, input)
			var taskID, projectID, runnerID, path, branch, base, state, setup, copies, association, executionBranch, executionState, taskState string
			err := f.db.QueryRow(`SELECT w.task_id,w.project_id,w.runner_id,w.path,w.branch_name,w.base_sha,w.status,w.setup_status,w.copied_files,e.worktree_id,e.branch_name,e.status,t.status
 FROM executions e JOIN tasks t ON t.id=e.task_id JOIN worktrees w ON w.id=e.worktree_id WHERE e.id=?`, id).
				Scan(&taskID, &projectID, &runnerID, &path, &branch, &base, &state, &setup, &copies, &association, &executionBranch, &executionState, &taskState)
			if err != nil || taskID == "" || projectID != "project-a" || runnerID != "claim-runner" || path != w["path"] || branch != w["branchName"] || base != w["baseSha"] || state != "active" || setup != "pending" || association != w["id"] || executionBranch != branch || executionState != "preparing" || taskState != "running" {
				t.Fatalf("created row: %s %s %s %s %s %s %s %s %s %s %s %s err=%v", taskID, projectID, runnerID, path, branch, base, state, setup, association, executionBranch, executionState, taskState, err)
			}
			var manifest []map[string]string
			if err := json.Unmarshal([]byte(copies), &manifest); err != nil || !reflect.DeepEqual(manifest, w["copiedFiles"]) {
				t.Fatalf("copies=%s err=%v", copies, err)
			}
			if got := stageEvent(t, f, id, "worktree_ready"); !reflect.DeepEqual(got, map[string]any{"path": path, "branch": branch, "baseSha": base}) {
				t.Fatalf("ready=%v", got)
			}
			skip := stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='log' AND json_extract(payload,'$.sourceId')=? AND json_extract(payload,'$.stream')='runner'`, id, "worktree-setup-not-required/"+association)
			if (command == "" && skip != 1) || (command != "" && skip != 0) {
				t.Fatalf("setup skip=%d", skip)
			}
			events := stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=?`, id)
			audits := stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE target_id=?`, id)
			for i := 0; i < 2; i++ {
				stageOK(t, f, token, id, input)
			}
			if stageCount(t, f, `SELECT count(*) FROM worktrees`) != before+1 || stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=?`, id) != events || stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE target_id=?`, id) != audits {
				t.Fatal("replay duplicated rows/events/audits")
			}
			if stageCount(t, f, `SELECT count(*) FROM notification_requests WHERE execution_id=?`, id) != 0 || stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='status_changed' AND json_extract(payload,'$.reason')='runner_result'`, id) != 0 {
				t.Fatal("stage report emitted transition side effects")
			}
			w["copiedFiles"] = []map[string]string{}
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 409, Conflict)
		})
	}
}

func TestStageCreatedRejectsDifferentBaseWithoutWrites(t *testing.T) {
	f, token, id, input := createdReport(t)
	input["worktree"].(map[string]any)["baseSha"] = strings.Repeat("c", 40)
	before := stageCount(t, f, `SELECT count(*) FROM worktrees`)
	events := stageCount(t, f, `SELECT count(*) FROM execution_events`)
	audits := stageCount(t, f, `SELECT count(*) FROM audit_logs`)
	status, body := postResult(t, f, token, id, input)
	assertAuthError(t, status, body, 422, PreconditionFailed)
	var w sql.NullString
	if err := f.db.QueryRow(`SELECT worktree_id FROM executions WHERE id=?`, id).Scan(&w); err != nil || w.Valid {
		t.Fatalf("association=%v err=%v", w, err)
	}
	if stageCount(t, f, `SELECT count(*) FROM worktrees`) != before || stageCount(t, f, `SELECT count(*) FROM execution_events`) != events || stageCount(t, f, `SELECT count(*) FROM audit_logs`) != audits {
		t.Fatal("base mismatch saved data")
	}
}

func restoreReport(t *testing.T) (*authFixture, string, string, string) {
	t.Helper()
	f, token, old, w := resultFixture(t, "preparing")
	stageExec(t, f, `UPDATE executions SET status='failed',base_sha=?,branch_name='result' WHERE id=?`, strings.Repeat("a", 40), old)
	stageExec(t, f, `UPDATE worktrees SET status='cleaned',setup_status='succeeded',copied_files=? WHERE id=?`, `[{"path":"old","sha256":"`+strings.Repeat("c", 64)+`"}]`, w)
	id := ulid.Make().String()
	stageExec(t, f, `INSERT INTO executions(id,task_id,seq,mode,status,runner_id,created_at) SELECT ?,task_id,2,'continue','preparing',runner_id,created_at FROM executions WHERE id=?`, id, old)
	return f, token, id, w
}
func restoreBody(w, action string) map[string]any {
	return map[string]any{"fromStatus": "preparing", "status": "preparing", "worktree": map[string]any{"action": action, "id": w}}
}

func TestStageRestoreInvalidatesThenRecordsCopiesAndCanReuse(t *testing.T) {
	f, token, id, w := restoreReport(t)
	start := restoreBody(w, "restore_started")
	stageOK(t, f, token, id, start)
	var copies, setup, state string
	if err := f.db.QueryRow(`SELECT copied_files,setup_status,status FROM worktrees WHERE id=?`, w).Scan(&copies, &setup, &state); err != nil || copies != "[]" || setup != "pending" || state != "cleaned" {
		t.Fatalf("restore start: %s %s %s %v", copies, setup, state, err)
	}
	log := stageEvent(t, f, id, "log")
	if log["sourceId"] != "worktree-restore/"+w || log["stream"] != "runner" {
		t.Fatalf("restore log=%v", log)
	}
	status, body := postResult(t, f, token, id, map[string]any{"fromStatus": "preparing", "status": "preparing", "worktreeId": w, "baseSha": strings.Repeat("a", 40), "branchName": "result"})
	assertAuthError(t, status, body, 422, PreconditionFailed)
	restored := restoreBody(w, "restored")
	data := restored["worktree"].(map[string]any)
	data["path"], data["branchName"], data["baseSha"], data["copiedFiles"] = "/worktrees/result", "result", strings.Repeat("a", 40), []map[string]string{{"path": "new.config", "sha256": strings.Repeat("d", 64)}}
	stageOK(t, f, token, id, restored)
	var association sql.NullString
	if err := f.db.QueryRow(`SELECT worktree_id FROM executions WHERE id=?`, id).Scan(&association); err != nil || association.Valid {
		t.Fatalf("restored associated early: %v %v", association, err)
	}
	if err := f.db.QueryRow(`SELECT copied_files FROM worktrees WHERE id=?`, w).Scan(&copies); err != nil || !strings.Contains(copies, "new.config") {
		t.Fatalf("manifest=%s err=%v", copies, err)
	}
	if got := stageEvent(t, f, id, "worktree_ready"); got["path"] != "/worktrees/result" || got["branch"] != "result" || got["baseSha"] != strings.Repeat("a", 40) {
		t.Fatalf("ready=%v", got)
	}
	events := stageCount(t, f, `SELECT count(*) FROM execution_events`)
	stageOK(t, f, token, id, start)
	stageOK(t, f, token, id, restored)
	if stageCount(t, f, `SELECT count(*) FROM execution_events`) != events {
		t.Fatal("restore replay duplicated events")
	}
	if err := f.db.QueryRow(`SELECT copied_files FROM worktrees WHERE id=?`, w).Scan(&copies); err != nil || !strings.Contains(copies, "new.config") {
		t.Fatal("start replay erased completed copies")
	}
	stageOK(t, f, token, id, map[string]any{"fromStatus": "preparing", "status": "preparing", "worktreeId": w, "baseSha": strings.Repeat("a", 40), "branchName": "result"})
	if err := f.db.QueryRow(`SELECT status FROM worktrees WHERE id=?`, w).Scan(&state); err != nil || state != "active" {
		t.Fatalf("reuse=%s %v", state, err)
	}
	data["path"] = "/different"
	status, body = postResult(t, f, token, id, restored)
	assertAuthError(t, status, body, 422, PreconditionFailed)
	data["path"] = "/worktrees/result"
	data["copiedFiles"] = []map[string]string{}
	status, body = postResult(t, f, token, id, restored)
	assertAuthError(t, status, body, 409, Conflict)
}

func TestStageRestoreRejectsNonPredecessor(t *testing.T) {
	for _, change := range []string{"wrong_id", "fresh", "older_execution", "other_runner", "other_project", "different_base"} {
		t.Run(change, func(t *testing.T) {
			f, token, id, w := restoreReport(t)
			switch change {
			case "wrong_id":
				w = ulid.Make().String()
			case "fresh":
				stageExec(t, f, `UPDATE executions SET mode='fresh' WHERE id=?`, id)
			case "older_execution":
				stageExec(t, f, `UPDATE executions SET seq=3 WHERE id=?`, id)
			case "other_runner":
				stageExec(t, f, `UPDATE worktrees SET runner_id='mac-mini' WHERE id=?`, w)
			case "other_project":
				stageExec(t, f, `UPDATE worktrees SET project_id='project-b' WHERE id=?`, w)
			case "different_base":
				stageExec(t, f, `UPDATE executions SET base_sha=? WHERE id=?`, strings.Repeat("b", 40), id)
			}
			events := stageCount(t, f, `SELECT count(*) FROM execution_events`)
			status, body := postResult(t, f, token, id, restoreBody(w, "restore_started"))
			assertAuthError(t, status, body, 422, PreconditionFailed)
			if stageCount(t, f, `SELECT count(*) FROM execution_events`) != events {
				t.Fatal("rejected restore wrote events")
			}
		})
	}
}

func publishReport(t *testing.T) (*authFixture, string, string, map[string]any) {
	t.Helper()
	f, token, id, _ := resultFixture(t, "publishing")
	stageExec(t, f, `UPDATE executions SET branch_name='evaloom/result',verified_sha=? WHERE id=?`, strings.Repeat("b", 40), id)
	return f, token, id, map[string]any{"fromStatus": "publishing", "status": "publishing", "pushedSha": strings.Repeat("b", 40), "branchName": "evaloom/result"}
}
func TestStagePushValidatesAndPersistsWithoutTransition(t *testing.T) {
	f, token, id, input := publishReport(t)
	before := readResultSnapshot(t, f, id)
	for _, field := range []string{"pushedSha", "branchName"} {
		previous := input[field]
		input[field] = strings.Repeat("c", 40)
		status, body := postResult(t, f, token, id, input)
		assertAuthError(t, status, body, 422, PreconditionFailed)
		if after := readResultSnapshot(t, f, id); after != before {
			t.Fatalf("rejected push mutated DB: %+v", after)
		}
		input[field] = previous
	}
	stageOK(t, f, token, id, input)
	stageOK(t, f, token, id, input)
	var pushed, state, taskState string
	if err := f.db.QueryRow(`SELECT e.pushed_sha,e.status,t.status FROM executions e JOIN tasks t ON t.id=e.task_id WHERE e.id=?`, id).Scan(&pushed, &state, &taskState); err != nil || pushed != strings.Repeat("b", 40) || state != "publishing" || taskState != "verifying" {
		t.Fatalf("push=%s %s %s %v", pushed, state, taskState, err)
	}
	if stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='pushed'`, id) != 1 {
		t.Fatal("push event duplicated")
	}
	if got := stageEvent(t, f, id, "pushed"); !reflect.DeepEqual(got, map[string]any{"sha": pushed, "branch": "evaloom/result"}) {
		t.Fatalf("pushed=%v", got)
	}
	after := readResultSnapshot(t, f, id)
	if after.Requests != before.Requests || after.Events != before.Events+1 || after.Audits != before.Audits+1 {
		t.Fatalf("unexpected side effects: %+v -> %+v", before, after)
	}
}

func TestStagePRRequiresSavedPushAndCreatesEventOnlyForNewPR(t *testing.T) {
	for _, created := range []bool{false, true} {
		t.Run(fmt.Sprint(created), func(t *testing.T) {
			f, token, id, push := publishReport(t)
			input := map[string]any{"fromStatus": "publishing", "status": "publishing", "prNumber": 12, "prUrl": "https://github.com/example/repo/pull/12", "prCreated": created}
			before := readResultSnapshot(t, f, id)
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 422, PreconditionFailed)
			if after := readResultSnapshot(t, f, id); after != before {
				t.Fatal("PR without push saved data")
			}
			stageOK(t, f, token, id, push)
			stageOK(t, f, token, id, input)
			stageOK(t, f, token, id, input)
			var number int
			var url, state string
			if err := f.db.QueryRow(`SELECT pr_number,pr_url,status FROM executions WHERE id=?`, id).Scan(&number, &url, &state); err != nil || number != 12 || url != input["prUrl"] || state != "publishing" {
				t.Fatalf("PR=%d %s %s %v", number, url, state, err)
			}
			want := 0
			if created {
				want = 1
			}
			if stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='pr_created'`, id) != want {
				t.Fatal("incorrect PR creation event count")
			}
			if created {
				if got := stageEvent(t, f, id, "pr_created"); !reflect.DeepEqual(got, map[string]any{"prNumber": float64(12), "prUrl": url}) {
					t.Fatalf("PR event=%v", got)
				}
			}
			input["prCreated"] = !created
			status, body = postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 409, Conflict)
			input["prCreated"] = created
			input["prNumber"] = 13
			status, body = postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 409, Conflict)
		})
	}
}

func TestStageReportsRollbackAllWrites(t *testing.T) {
	for _, kind := range []string{"created", "restore_started", "restored", "push", "pr"} {
		t.Run(kind, func(t *testing.T) {
			for _, target := range []string{"data", "event", "audit"} {
				t.Run(target, func(t *testing.T) {
					var f *authFixture
					var token, id string
					var input map[string]any
					var dataTrigger string
					switch kind {
					case "created":
						f, token, id, input = createdReport(t)
						dataTrigger = `BEFORE UPDATE OF worktree_id ON executions`
					case "restore_started", "restored":
						var w string
						f, token, id, w = restoreReport(t)
						input = restoreBody(w, kind)
						if kind == "restored" {
							stageOK(t, f, token, id, restoreBody(w, "restore_started"))
							data := input["worktree"].(map[string]any)
							data["path"], data["branchName"], data["baseSha"], data["copiedFiles"] = "/worktrees/result", "result", strings.Repeat("a", 40), []map[string]string{{"path": "new", "sha256": strings.Repeat("b", 64)}}
						}
						dataTrigger = `BEFORE UPDATE OF copied_files ON worktrees`
					case "push":
						f, token, id, input = publishReport(t)
						dataTrigger = `BEFORE UPDATE OF pushed_sha ON executions`
					case "pr":
						var push map[string]any
						f, token, id, push = publishReport(t)
						stageOK(t, f, token, id, push)
						input = map[string]any{"fromStatus": "publishing", "status": "publishing", "prNumber": 12, "prUrl": "https://github.com/example/repo/pull/12", "prCreated": true}
						dataTrigger = `BEFORE UPDATE OF pr_number ON executions`
					}
					snapshot := func() string {
						var data string
						if err := f.db.QueryRow(`SELECT json_object('executions',(SELECT json_group_array(json_object('id',id,'worktree',worktree_id,'branch',branch_name,'pushed',pushed_sha,'pr',pr_number,'url',pr_url,'status',status)) FROM executions),
 'worktrees',(SELECT json_group_array(json_object('id',id,'copies',copied_files,'setup',setup_status,'status',status)) FROM worktrees),'events',(SELECT count(*) FROM execution_events),'audits',(SELECT count(*) FROM audit_logs),'notifications',(SELECT count(*) FROM notification_requests))`).Scan(&data); err != nil {
							t.Fatal(err)
						}
						return data
					}
					before := snapshot()
					trigger := dataTrigger
					if target == "event" {
						trigger = `BEFORE INSERT ON execution_events`
					}
					if target == "audit" {
						trigger = `BEFORE INSERT ON audit_logs WHEN NEW.action='execution.result'`
					}
					stageExec(t, f, `CREATE TRIGGER reject_stage `+trigger+` BEGIN SELECT RAISE(ABORT,'injected stage storage failure'); END`)
					status, body := postResult(t, f, token, id, input)
					assertAuthError(t, status, body, 500, Internal)
					if after := snapshot(); after != before {
						t.Fatalf("partial stage survived: before=%s after=%s", before, after)
					}
				})
			}
		})
	}
}

func TestStageRejectsMixedRowsAndInvalidMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"gate", func(input map[string]any) { input["gateResults"] = []map[string]any{{"gate": 3, "result": "pass"}} }},
		{"error", func(input map[string]any) { input["errorClass"] = "config_error" }},
		{"push", func(input map[string]any) { input["pushedSha"] = strings.Repeat("b", 40) }},
		{"pr", func(input map[string]any) { input["prCreated"] = false }},
		{"transition", func(input map[string]any) { input["status"] = "running" }},
		{"unknown_action", func(input map[string]any) { input["worktree"].(map[string]any)["action"] = "deleted" }},
		{"relative_path", func(input map[string]any) { input["worktree"].(map[string]any)["path"] = "relative" }},
		{"missing_copies", func(input map[string]any) { delete(input["worktree"].(map[string]any), "copiedFiles") }},
		{"copy_escape", func(input map[string]any) {
			input["worktree"].(map[string]any)["copiedFiles"] = []map[string]string{{"path": "../escape", "sha256": strings.Repeat("a", 64)}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, id, input := createdReport(t)
			tc.edit(input)
			before := stageCount(t, f, `SELECT count(*) FROM worktrees`)
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 400, ValidationFailed)
			if stageCount(t, f, `SELECT count(*) FROM worktrees`) != before {
				t.Fatal("invalid report wrote worktree")
			}
		})
	}
}

func TestResultFailureAuditAtomicWithTransition(t *testing.T) {
	for _, scenario := range []string{"worktree.path_rejected", "publish.remote_mismatch", "remote_branch_missing"} {
		t.Run(scenario, func(t *testing.T) {
			action := scenario
			if scenario == "remote_branch_missing" {
				action = "publish.remote_mismatch"
			}
			for _, failure := range []string{"", "audit", "transition"} {
				t.Run(failure, func(t *testing.T) {
					f, token, id, _ := resultFixture(t, "running")
					input := resultBody("running", "failed")
					detail := map[string]any{"path": "/outside/root"}
					input["errorClass"] = "config_error"
					if action == "publish.remote_mismatch" {
						input["errorClass"] = "publish_failed"
						detail = map[string]any{"branch": "evaloom/task", "verifiedSha": strings.Repeat("a", 40), "observedSha": strings.Repeat("b", 40)}
						if scenario == "remote_branch_missing" {
							detail["observedSha"] = ""
						}
					}
					input["audit"] = map[string]any{"action": action, "detail": detail}
					before := readResultSnapshot(t, f, id)
					if failure == "audit" {
						stageExec(t, f, `CREATE TRIGGER reject_failure_audit BEFORE INSERT ON audit_logs WHEN NEW.action='`+action+`' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
					}
					if failure == "transition" {
						stageExec(t, f, `CREATE TRIGGER reject_failure_transition BEFORE UPDATE OF status ON tasks BEGIN SELECT RAISE(ABORT,'transition unavailable'); END`)
					}
					status, body := postResult(t, f, token, id, input)
					if failure != "" {
						assertAuthError(t, status, body, 500, Internal)
						if after := readResultSnapshot(t, f, id); after != before {
							t.Fatalf("partial transition: %+v -> %+v", before, after)
						}
						if stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE action=?`, action) != 0 {
							t.Fatal("failure audit survived rollback")
						}
						return
					}
					if status != 204 {
						t.Fatalf("failed audit=%d %s", status, body)
					}
					after := readResultSnapshot(t, f, id)
					if after.Execution != "failed" || after.Task != "failed" || after.Worktree != "retained" || after.Requests != before.Requests+1 {
						t.Fatalf("failed state=%+v", after)
					}
					var actor, targetType, targetID, data string
					if err := f.db.QueryRow(`SELECT actor,target_type,target_id,detail FROM audit_logs WHERE action=?`, action).Scan(&actor, &targetType, &targetID, &data); err != nil || actor != "runner:claim-runner" || targetType != "execution" || targetID != id {
						t.Fatalf("audit=%s %s %s err=%v", actor, targetType, targetID, err)
					}
					var saved map[string]any
					if err := json.Unmarshal([]byte(data), &saved); err != nil || !reflect.DeepEqual(saved, detail) {
						t.Fatalf("audit detail=%s err=%v", data, err)
					}
				})
			}
		})
	}
}

func TestResultRejectsInvalidFailureAuditWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, action, class string
		detail                        map[string]any
	}{
		{"progress", "running", "verifying", "worktree.path_rejected", "", map[string]any{"path": "/outside"}},
		{"timeout", "running", "timed_out", "worktree.path_rejected", "timeout", map[string]any{"path": "/outside"}},
		{"unknown", "running", "failed", "arbitrary.action", "config_error", map[string]any{"path": "/outside"}},
		{"class", "running", "failed", "worktree.path_rejected", "execution_failed", map[string]any{"path": "/outside"}},
		{"extra_detail", "running", "failed", "worktree.path_rejected", "config_error", map[string]any{"path": "/outside", "rawError": "secret"}},
		{"missing_detail", "running", "failed", "publish.remote_mismatch", "publish_failed", map[string]any{"branch": "evaloom/result"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, tc.from)
			before := readResultSnapshot(t, f, id)
			input := resultBody(tc.from, tc.to)
			input["errorClass"] = tc.class
			input["audit"] = map[string]any{"action": tc.action, "detail": tc.detail}
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 400, ValidationFailed)
			if after := readResultSnapshot(t, f, id); after != before {
				t.Fatal("invalid audit changed state")
			}
		})
	}
}

func TestResultSavesHarnessSessionOnLeavingRunning(t *testing.T) {
	for _, target := range []string{"verifying", "failed", "timed_out", "cancelled"} {
		t.Run(target, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "running")
			input := resultBody("running", target)
			input["harnessSessionId"] = "adapter-session-42"
			stageOK(t, f, token, id, input)
			var session, state string
			if err := f.db.QueryRow(`SELECT harness_session_id,status FROM executions WHERE id=?`, id).Scan(&session, &state); err != nil || session != "adapter-session-42" || state != target {
				t.Fatalf("session=%s state=%s err=%v", session, state, err)
			}
		})
	}
}
