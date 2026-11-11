package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
)

// reportFixture claims a task on project-a. The Execution is claimed.
func reportFixture(t *testing.T) (*authFixture, string, string, string) {
	t.Helper()
	f, token, _ := claimFixture(t)
	createClaimTask(t, f, "project-a", "result")
	status, body := requestClaim(t, f, token)
	claim := requireClaim(t, status, body)
	return f, token, claim.Execution.ID, claim.Execution.TaskID
}

func postReport(t *testing.T, f *authFixture, token, id string, input any) (int, []byte) {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return postReportRaw(t, f, token, id, string(data))
}

func postReportRaw(t *testing.T, f *authFixture, token, id, data string) (int, []byte) {
	t.Helper()
	return f.request(t, "POST", "/api/executions/"+id+"/result", false, "", func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Body = io.NopCloser(strings.NewReader(data))
	})
}

// addArtifact registers an artifact row as the artifacts API would.
func addArtifact(t *testing.T, f *authFixture, executionID, stage, kind, name string) string {
	t.Helper()
	id := ulid.Make().String()
	execSQL(t, f, `INSERT INTO artifacts (id, execution_id, stage, kind, name, storage_path, sha256, size_bytes, redacted, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 1, '2026-11-11T00:00:00Z')`, id, executionID, stage, kind, name, "artifacts/"+id, strings.Repeat("0", 64))
	return id
}

// setDefinition changes the pinned Profile definition at a JSON path.
func setDefinition(t *testing.T, f *authFixture, executionID, path, value string) {
	t.Helper()
	execSQL(t, f, `UPDATE harness_profiles SET definition = json_set(definition, ?, json(?))
		WHERE id = (SELECT profile_id FROM executions WHERE id = ?)`, path, value, executionID)
}

func started(stage, artifact string) map[string]any {
	return map[string]any{"type": "stage_started", "stage": stage, "requestArtifactId": artifact}
}

func finished(stage string, attempt int, result any) map[string]any {
	input := map[string]any{
		"type": "stage_finished", "stage": stage, "attempt": attempt, "exitCode": 0, "runnerError": nil,
		"result": result, "resultArtifactId": nil, "work": nil,
		"usage": map[string]any{"wallTimeMs": 100, "inputTokens": nil, "outputTokens": nil, "costAmount": nil},
	}
	if stage == "work" {
		input["result"] = nil
		input["work"] = map[string]any{"sessionId": "session-1", "summaryArtifactId": nil, "question": nil}
	}
	return input
}

func programResult(stage, status string, extra map[string]any) map[string]any {
	result := map[string]any{"contractVersion": "1", "stage": stage, "status": status, "errorClass": nil, "message": "done", "usage": nil}
	if stage == "check" && status != "failed" {
		result["contentId"] = "content-1"
	}
	for k, v := range extra {
		result[k] = v
	}
	return result
}

// startStage reports stage_started and returns the attempt.
func startStage(t *testing.T, f *authFixture, token, id, stage string) int {
	t.Helper()
	status, body := postReport(t, f, token, id, started(stage, addArtifact(t, f, id, stage, "request_json", "request.json")))
	var reply struct{ Attempt int }
	if status != 200 || json.Unmarshal(body, &reply) != nil || reply.Attempt < 1 {
		t.Fatalf("start %s: %d %s", stage, status, body)
	}
	return reply.Attempt
}

type stageReply struct {
	Next             string  `json:"next"`
	CheckedContentID *string `json:"checkedContentId"`
	Approvals        []struct {
		ID        *string `json:"id"`
		Operation string  `json:"operation"`
		ContentID *string `json:"contentId"`
		Status    string  `json:"status"`
	} `json:"approvals"`
}

func finishStage(t *testing.T, f *authFixture, token, id string, input map[string]any) (stageReply, []byte) {
	t.Helper()
	status, body := postReport(t, f, token, id, input)
	var reply stageReply
	if status != 200 || json.Unmarshal(body, &reply) != nil || reply.Next == "" {
		t.Fatalf("finish %v: %d %s", input["stage"], status, body)
	}
	return reply, body
}

// runTo starts and finishes the stages before stage with success, then
// starts stage. It returns its attempt.
func runTo(t *testing.T, f *authFixture, token, id, stage string) int {
	t.Helper()
	for _, s := range []string{"work", "check", "publish"} {
		attempt := startStage(t, f, token, id, s)
		if s == stage {
			return attempt
		}
		var result any
		if s != "work" {
			result = programResult(s, "succeeded", nil)
		}
		finishStage(t, f, token, id, finished(s, attempt, result))
	}
	t.Fatalf("unknown stage %s", stage)
	return 0
}

type executionState struct {
	Execution, Task string
	ErrorClass      sql.NullString
	ResumeStage     sql.NullString
	ContentID       sql.NullString
}

func readState(t *testing.T, f *authFixture, id string) executionState {
	t.Helper()
	var s executionState
	if err := f.db.QueryRow(`SELECT e.status, t.status, e.error_class, e.resume_stage, e.content_id FROM executions e
		JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, id).Scan(&s.Execution, &s.Task, &s.ErrorClass, &s.ResumeStage, &s.ContentID); err != nil {
		t.Fatal(err)
	}
	return s
}

func requireState(t *testing.T, f *authFixture, id, execution, task, errorClass string) {
	t.Helper()
	s := readState(t, f, id)
	if s.Execution != execution || s.Task != task || s.ErrorClass.String != errorClass {
		t.Fatalf("state=%+v; want %s/%s/%s", s, execution, task, errorClass)
	}
}

func count(t *testing.T, f *authFixture, query string, args ...any) int {
	t.Helper()
	return stageCount(t, f, query, args...)
}

func stageRunStatus(t *testing.T, f *authFixture, id, stage string) (string, sql.NullString) {
	t.Helper()
	var status string
	var class sql.NullString
	if err := f.db.QueryRow(`SELECT status, error_class FROM stage_runs WHERE execution_id = ? AND stage = ? ORDER BY attempt DESC LIMIT 1`,
		id, stage).Scan(&status, &class); err != nil {
		t.Fatal(err)
	}
	return status, class
}

func openDecisionOf(t *testing.T, f *authFixture, id string) (kind, cause, basis string) {
	t.Helper()
	var c, b sql.NullString
	if err := f.db.QueryRow(`SELECT kind, cause, basis FROM owner_decisions WHERE execution_id = ? AND status = 'open'`, id).
		Scan(&kind, &c, &b); err != nil {
		t.Fatal(err)
	}
	return kind, c.String, b.String
}

func approvalDeclarations(t *testing.T, f *authFixture, id string) {
	t.Helper()
	setDefinition(t, f, id, "$.approvals", `[{"operation":"merge_pull_request","description":"Merge"},
		{"operation":"deploy","description":"Deploy","waivable":true}]`)
}

func stageApprovalRequest(operation string) map[string]any {
	return map[string]any{"operation": operation, "contentId": "content-1",
		"target": map[string]any{"label": "PR #1", "url": "https://example.com/1"}, "summary": "Do " + operation}
}

// spec 04 3.4.1: a stage start creates a running stage run, moves the
// Execution and returns the attempt; work starts first without prepare.
func TestStageStartedMovesExecutionAndReturnsAttempt(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	artifact := addArtifact(t, f, id, "work", "request_json", "request.json")
	status, body := postReport(t, f, token, id, started("work", artifact))
	if status != 200 || strings.TrimSpace(string(body)) != `{"attempt":1}` {
		t.Fatalf("start: %d %s", status, body)
	}
	requireState(t, f, id, "working", "running", "")
	if s, _ := stageRunStatus(t, f, id, "work"); s != "running" {
		t.Fatalf("stage run=%s", s)
	}
	if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = 'stage_started'
		AND json_extract(payload, '$.stage') = 'work' AND json_extract(payload, '$.attempt') = 1`, id); n != 1 {
		t.Fatalf("stage_started events=%d", n)
	}
	var startedAt sql.NullString
	if err := f.db.QueryRow(`SELECT started_at FROM executions WHERE id = ?`, id).Scan(&startedAt); err != nil || !startedAt.Valid {
		t.Fatalf("started_at=%v err=%v", startedAt, err)
	}
	// A resend returns the same attempt without new rows or events.
	events := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id = ?`, id)
	status, body = postReport(t, f, token, id, started("work", artifact))
	if status != 200 || !strings.Contains(string(body), `"attempt":1`) ||
		count(t, f, `SELECT count(*) FROM stage_runs WHERE execution_id = ?`, id) != 1 ||
		count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id = ?`, id) != events {
		t.Fatalf("resent start: %d %s", status, body)
	}
	// The same request artifact for another stage is a conflict.
	status, body = postReport(t, f, token, id, started("check", artifact))
	assertAuthError(t, status, body, 409, Conflict)
	// A request artifact of another kind is invalid.
	status, body = postReport(t, f, token, id, started("check", addArtifact(t, f, id, "check", "log", "log")))
	assertAuthError(t, status, body, 400, ValidationFailed)
}

// spec 04 3.4.1: prepare is 409 without its declaration; with it the
// Execution starts at prepare; a stage out of order and a stage other than
// resume_stage are 409.
func TestStageStartedFollowsProfileAndOrder(t *testing.T) {
	t.Run("prepare_undeclared", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		status, body := postReport(t, f, token, id, started("prepare", addArtifact(t, f, id, "prepare", "request_json", "request.json")))
		assertAuthError(t, status, body, 409, Conflict)
		requireState(t, f, id, "claimed", "running", "")
		if n := count(t, f, `SELECT count(*) FROM stage_runs WHERE execution_id = ?`, id); n != 0 {
			t.Fatalf("stage runs=%d", n)
		}
	})
	t.Run("prepare_declared", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		setDefinition(t, f, id, "$.stages.prepare", `{"command":["evaloom/prepare/run"],"timeoutSec":60}`)
		status, body := postReport(t, f, token, id, started("work", addArtifact(t, f, id, "work", "request_json", "request.json")))
		assertAuthError(t, status, body, 409, Conflict)
		startStage(t, f, token, id, "prepare")
		requireState(t, f, id, "preparing", "running", "")
	})
	t.Run("out_of_order", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		for _, stage := range []string{"check", "publish"} {
			status, body := postReport(t, f, token, id, started(stage, addArtifact(t, f, id, stage, "request_json", "request.json")))
			assertAuthError(t, status, body, 409, Conflict)
		}
		startStage(t, f, token, id, "work")
		// check cannot start before work finished.
		status, body := postReport(t, f, token, id, started("check", addArtifact(t, f, id, "check", "request_json", "request.json")))
		assertAuthError(t, status, body, 409, Conflict)
	})
	t.Run("resume_stage", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		execSQL(t, f, `UPDATE executions SET resume_stage = 'publish' WHERE id = ?`, id)
		status, body := postReport(t, f, token, id, started("work", addArtifact(t, f, id, "work", "request_json", "request.json")))
		assertAuthError(t, status, body, 409, Conflict)
		startStage(t, f, token, id, "publish")
		requireState(t, f, id, "publishing", "running", "")
	})
}

// spec 04 3.4.1, the table: the rows of a stage end.
func TestStageFinishedTableRows(t *testing.T) {
	t.Run("workspace_missing_first", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		attempt := startStage(t, f, token, id, "work")
		input := finished("work", attempt, nil)
		input["runnerError"], input["work"] = "workspace_missing", nil
		if reply, _ := finishStage(t, f, token, id, input); reply.Next != "stop" {
			t.Fatalf("next=%s", reply.Next)
		}
		requireState(t, f, id, "failed", "failed", "config_error")
	})
	t.Run("workspace_missing_resumed", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		execSQL(t, f, `UPDATE executions SET resume_stage = 'work' WHERE id = ?`, id)
		attempt := startStage(t, f, token, id, "work")
		input := finished("work", attempt, nil)
		input["runnerError"], input["work"] = "workspace_missing", nil
		if reply, _ := finishStage(t, f, token, id, input); reply.Next != "stop" {
			t.Fatalf("next=%s", reply.Next)
		}
		requireState(t, f, id, "blocked", "blocked", "")
		if kind, cause, _ := openDecisionOf(t, f, id); kind != "blocked" || cause != "workspace_missing" {
			t.Fatalf("decision=%s/%s", kind, cause)
		}
	})
	for _, tc := range []struct{ runnerError, execution, task, class, run string }{
		{"timeout", "timed_out", "timed_out", "timeout", "timed_out"},
		{"cancelled", "cancelled", "cancelled", "cancelled", "cancelled"},
		{"sandbox_error", "failed", "failed", "sandbox_error", "failed"},
		{"work_failed", "failed", "failed", "work_failed", "failed"},
	} {
		t.Run("runner_error_"+tc.runnerError, func(t *testing.T) {
			f, token, id, _ := reportFixture(t)
			attempt := startStage(t, f, token, id, "work")
			input := finished("work", attempt, nil)
			input["runnerError"], input["exitCode"], input["work"] = tc.runnerError, 1, nil
			if reply, _ := finishStage(t, f, token, id, input); reply.Next != "stop" {
				t.Fatalf("next=%s", reply.Next)
			}
			requireState(t, f, id, tc.execution, tc.task, tc.class)
			if run, class := stageRunStatus(t, f, id, "work"); run != tc.run || class.String != tc.class {
				t.Fatalf("stage run=%s/%v", run, class)
			}
		})
	}
	t.Run("result_failed", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		attempt := runTo(t, f, token, id, "publish")
		reply, _ := finishStage(t, f, token, id, finished("publish", attempt,
			programResult("publish", "failed", map[string]any{"errorClass": "publish_failed"})))
		if reply.Next != "stop" {
			t.Fatalf("next=%s", reply.Next)
		}
		requireState(t, f, id, "failed", "failed", "publish_failed")
	})
	t.Run("prepare_succeeded", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		setDefinition(t, f, id, "$.stages.prepare", `{"command":["evaloom/prepare/run"],"timeoutSec":60}`)
		attempt := startStage(t, f, token, id, "prepare")
		reply, _ := finishStage(t, f, token, id, finished("prepare", attempt, programResult("prepare", "succeeded", map[string]any{
			"reportedContext": []any{map[string]any{"source": "repo", "path": "AGENTS.md", "revision": "abc", "sha256": strings.Repeat("a", 64)}},
		})))
		if reply.Next != "work" {
			t.Fatalf("next=%s", reply.Next)
		}
		if n := count(t, f, `SELECT count(*) FROM context_revisions WHERE execution_id = ? AND layer = 'reported' AND kind = 'reported'
			AND source_path = 'repo:AGENTS.md' AND source_revision = 'abc' AND content IS NULL`, id); n != 1 {
			t.Fatalf("reported context=%d", n)
		}
		requireState(t, f, id, "preparing", "running", "")
		startStage(t, f, token, id, "work")
	})
	t.Run("work_question", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		attempt := startStage(t, f, token, id, "work")
		input := finished("work", attempt, nil)
		input["work"] = map[string]any{"sessionId": "session-9", "summaryArtifactId": nil,
			"question": map[string]any{"question": "Keep or remove?", "options": []string{"keep", "remove"}}}
		if reply, _ := finishStage(t, f, token, id, input); reply.Next != "stop" {
			t.Fatalf("next=%s", reply.Next)
		}
		requireState(t, f, id, "blocked", "blocked", "")
		if run, _ := stageRunStatus(t, f, id, "work"); run != "question" {
			t.Fatalf("stage run=%s", run)
		}
		if kind, cause, _ := openDecisionOf(t, f, id); kind != "question" || cause != "question" {
			t.Fatalf("decision=%s/%s", kind, cause)
		}
		var session string
		if err := f.db.QueryRow(`SELECT work_session_id FROM executions WHERE id = ?`, id).Scan(&session); err != nil || session != "session-9" {
			t.Fatalf("session=%s err=%v", session, err)
		}
	})
	t.Run("work_succeeded", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		attempt := startStage(t, f, token, id, "work")
		input := finished("work", attempt, nil)
		input["work"].(map[string]any)["summaryArtifactId"] = addArtifact(t, f, id, "work", "work_summary", "summary.md")
		if reply, _ := finishStage(t, f, token, id, input); reply.Next != "check" {
			t.Fatalf("next=%s", reply.Next)
		}
		requireState(t, f, id, "working", "running", "")
		if run, _ := stageRunStatus(t, f, id, "work"); run != "succeeded" {
			t.Fatalf("stage run=%s", run)
		}
	})
	t.Run("check_succeeded", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		attempt := runTo(t, f, token, id, "check")
		reply, body := finishStage(t, f, token, id, finished("check", attempt, programResult("check", "succeeded", nil)))
		if reply.Next != "publish" || reply.CheckedContentID == nil || *reply.CheckedContentID != "content-1" ||
			reply.Approvals == nil || len(reply.Approvals) != 0 {
			t.Fatalf("reply=%s", body)
		}
		if s := readState(t, f, id); s.ContentID.String != "content-1" || s.Execution != "checking" {
			t.Fatalf("state=%+v", s)
		}
		if n := count(t, f, `SELECT count(*) FROM check_results WHERE execution_id = ? AND result = 'pass' AND content_id = 'content-1'`, id); n != 1 {
			t.Fatalf("check results=%d", n)
		}
	})
	t.Run("check_all_waived", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		approvalDeclarations(t, f, id)
		execSQL(t, f, `UPDATE projects SET approval_waivers = '["deploy"]' WHERE id = 'project-a'`)
		attempt := runTo(t, f, token, id, "check")
		reply, body := finishStage(t, f, token, id, finished("check", attempt, programResult("check", "needs_approval", map[string]any{
			"approvalRequests": []any{stageApprovalRequest("deploy")},
		})))
		if reply.Next != "publish" || len(reply.Approvals) != 1 || reply.Approvals[0].ID != nil ||
			reply.Approvals[0].Operation != "deploy" || reply.Approvals[0].Status != "waived" {
			t.Fatalf("reply=%s", body)
		}
		if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = 'approval_waived'`, id); n != 1 {
			t.Fatalf("waived events=%d", n)
		}
		if n := count(t, f, `SELECT count(*) FROM approvals WHERE execution_id = ?`, id); n != 0 {
			t.Fatalf("approvals=%d", n)
		}
		startStage(t, f, token, id, "publish")
	})
	for _, tc := range []struct{ stage, status string }{{"check", "needs_approval"}, {"check", "succeeded"}, {"publish", "needs_approval"}} {
		t.Run("approval_"+tc.stage+"_"+tc.status, func(t *testing.T) {
			f, token, id, _ := reportFixture(t)
			approvalDeclarations(t, f, id)
			execSQL(t, f, `UPDATE projects SET approval_waivers = '["deploy"]' WHERE id = 'project-a'`)
			attempt := runTo(t, f, token, id, tc.stage)
			extra := map[string]any{"approvalRequests": []any{stageApprovalRequest("merge_pull_request"), stageApprovalRequest("deploy")}}
			if tc.stage == "publish" {
				extra["contentId"] = "content-1"
			}
			if reply, _ := finishStage(t, f, token, id, finished(tc.stage, attempt, programResult(tc.stage, tc.status, extra))); reply.Next != "stop" {
				t.Fatalf("next=%s", reply.Next)
			}
			requireState(t, f, id, "waiting_approval", "waiting_approval", "")
			var operation, status, basis, decisionStatus, requested string
			if err := f.db.QueryRow(`SELECT a.operation, a.status, d.basis, d.status, a.requested_stage FROM approvals a
				JOIN owner_decisions d ON d.id = a.decision_id AND d.kind = 'approval' AND d.subject_id = a.id
				WHERE a.execution_id = ?`, id).Scan(&operation, &status, &basis, &decisionStatus, &requested); err != nil {
				t.Fatal(err)
			}
			if operation != "merge_pull_request" || status != "pending" || basis != "content-1" || decisionStatus != "open" || requested != tc.stage {
				t.Fatalf("approval=%s %s %s %s %s", operation, status, basis, decisionStatus, requested)
			}
			for event, want := range map[string]int{"approval_requested": 1, "approval_waived": 1, "decision_opened": 1} {
				if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = ?`, id, event); n != want {
					t.Fatalf("%s events=%d", event, n)
				}
			}
			if n := count(t, f, `SELECT count(*) FROM notification_requests WHERE execution_id = ? AND status = 'waiting_approval'`, id); n != 1 {
				t.Fatalf("notification requests=%d", n)
			}
		})
	}
	t.Run("check_blocked", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		attempt := runTo(t, f, token, id, "check")
		finishStage(t, f, token, id, finished("check", attempt, programResult("check", "blocked", map[string]any{
			"findings": []any{map[string]any{"rule": "spec_mismatch", "severity": "error", "message": "differs"}},
		})))
		requireState(t, f, id, "blocked", "blocked", "")
		if kind, cause, basis := openDecisionOf(t, f, id); kind != "blocked" || cause != "check_blocked" || basis != "content-1" {
			t.Fatalf("decision=%s/%s/%s", kind, cause, basis)
		}
		if n := count(t, f, `SELECT count(*) FROM check_results WHERE execution_id = ? AND result = 'blocked'
			AND content_id = 'content-1' AND json_array_length(findings) = 1`, id); n != 1 {
			t.Fatalf("check results=%d", n)
		}
		if s := readState(t, f, id); s.ContentID.String != "content-1" {
			t.Fatalf("content=%v", s.ContentID)
		}
	})
	t.Run("check_failed", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		attempt := runTo(t, f, token, id, "check")
		finishStage(t, f, token, id, finished("check", attempt, programResult("check", "failed", map[string]any{"errorClass": "check_failed"})))
		requireState(t, f, id, "failed", "failed", "check_failed")
		if n := count(t, f, `SELECT count(*) FROM check_results WHERE execution_id = ? AND result = 'fail'`, id); n != 1 {
			t.Fatalf("check results=%d", n)
		}
	})
	t.Run("publish_succeeded", func(t *testing.T) {
		f, token, id, taskID := reportFixture(t)
		setDefinition(t, f, id, "$.records", `{"keep":["publish/report.md","check/missing.md"],"pass":{"recent":3}}`)
		attempt := runTo(t, f, token, id, "publish")
		artifact := addArtifact(t, f, id, "publish", "out_file", "report.md")
		outputs := []any{map[string]any{"kind": "pull_request", "label": "PR #1", "url": "https://example.com/1"}}
		reply, _ := finishStage(t, f, token, id, finished("publish", attempt, programResult("publish", "succeeded", map[string]any{"outputs": outputs})))
		if reply.Next != "stop" {
			t.Fatalf("next=%s", reply.Next)
		}
		requireState(t, f, id, "succeeded", "completed", "")
		var saved string
		if err := f.db.QueryRow(`SELECT outputs FROM executions WHERE id = ?`, id).Scan(&saved); err != nil ||
			!strings.Contains(saved, `"url":"https://example.com/1"`) {
			t.Fatalf("outputs=%s err=%v", saved, err)
		}
		if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = 'published'`, id); n != 1 {
			t.Fatalf("published=%d", n)
		}
		var fileName string
		if err := f.db.QueryRow(`SELECT file_name FROM harness_records WHERE execution_id = ? AND artifact_id = ? AND harness_id = 'harness-a'`,
			id, artifact).Scan(&fileName); err != nil || !strings.HasSuffix(fileName, "-"+taskID+"-report.md") {
			t.Fatalf("record=%s err=%v", fileName, err)
		}
		if n := count(t, f, `SELECT count(*) FROM harness_records WHERE execution_id = ?`, id); n != 1 {
			t.Fatalf("records=%d", n)
		}
		if n := count(t, f, `SELECT count(*) FROM notification_requests WHERE execution_id = ? AND status = 'succeeded'`, id); n != 1 {
			t.Fatalf("notification requests=%d", n)
		}
	})
	t.Run("publish_unknown", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		attempt := runTo(t, f, token, id, "publish")
		finishStage(t, f, token, id, finished("publish", attempt, programResult("publish", "unknown", map[string]any{"errorClass": "publish_unknown"})))
		requireState(t, f, id, "blocked", "blocked", "")
		if kind, cause, _ := openDecisionOf(t, f, id); kind != "blocked" || cause != "publish_unknown" {
			t.Fatalf("decision=%s/%s", kind, cause)
		}
		if run, class := stageRunStatus(t, f, id, "publish"); run != "unknown" || class.String != "publish_unknown" {
			t.Fatalf("stage run=%s/%v", run, class)
		}
	})
	t.Run("publish_all_waived", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		approvalDeclarations(t, f, id)
		execSQL(t, f, `UPDATE projects SET approval_waivers = '["deploy"]' WHERE id = 'project-a'`)
		attempt := runTo(t, f, token, id, "publish")
		reply, _ := finishStage(t, f, token, id, finished("publish", attempt, programResult("publish", "needs_approval", map[string]any{
			"contentId": "content-2", "approvalRequests": []any{stageApprovalRequest("deploy")},
		})))
		if reply.Next != "stop" {
			t.Fatalf("next=%s", reply.Next)
		}
		s := readState(t, f, id)
		if s.Execution != "pending" || s.Task != "queued" || s.ResumeStage.String != "publish" {
			t.Fatalf("state=%+v", s)
		}
		if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = 'approval_waived'`, id); n != 1 {
			t.Fatalf("waived events=%d", n)
		}
	})
}

// spec 04 3.4.1 step 1: an undeclared approval, a classification the
// special harness cannot write, and a check succeeded without contentId are
// contract_error.
func TestStageFinishedContractErrors(t *testing.T) {
	for name, result := range map[string]map[string]any{
		"undeclared_approval":  programResult("check", "needs_approval", map[string]any{"approvalRequests": []any{stageApprovalRequest("delete_repo")}}),
		"reserved_error_class": programResult("check", "failed", map[string]any{"errorClass": "work_failed"}),
		"missing_content_id":   programResult("check", "succeeded", map[string]any{"contentId": nil}),
		"unknown_field":        programResult("check", "succeeded", map[string]any{"extra": true}),
		"other_stage":          programResult("publish", "succeeded", nil),
	} {
		t.Run(name, func(t *testing.T) {
			f, token, id, _ := reportFixture(t)
			approvalDeclarations(t, f, id)
			attempt := runTo(t, f, token, id, "check")
			if reply, _ := finishStage(t, f, token, id, finished("check", attempt, result)); reply.Next != "stop" {
				t.Fatalf("next=%s", reply.Next)
			}
			requireState(t, f, id, "failed", "failed", "contract_error")
			if run, class := stageRunStatus(t, f, id, "check"); run != "failed" || class.String != "contract_error" {
				t.Fatalf("stage run=%s/%v", run, class)
			}
			if n := count(t, f, `SELECT count(*) FROM approvals WHERE execution_id = ?`, id); n != 0 {
				t.Fatalf("approvals=%d", n)
			}
		})
	}
	t.Run("invalid_question", func(t *testing.T) {
		f, token, id, _ := reportFixture(t)
		attempt := startStage(t, f, token, id, "work")
		input := finished("work", attempt, nil)
		input["work"].(map[string]any)["question"] = map[string]any{"question": ""}
		finishStage(t, f, token, id, input)
		requireState(t, f, id, "failed", "failed", "contract_error")
	})
}

type reportSnapshot struct {
	Execution, Task                                      string
	StageRuns, Events, Audits, Usage, Decisions, Results int
}

func readReportSnapshot(t *testing.T, f *authFixture, id string) reportSnapshot {
	t.Helper()
	var s reportSnapshot
	if err := f.db.QueryRow(`SELECT e.status, t.status,
		(SELECT count(*) FROM stage_runs WHERE execution_id = e.id),
		(SELECT count(*) FROM execution_events WHERE execution_id = e.id),
		(SELECT count(*) FROM audit_logs WHERE action = 'execution.result' AND target_id = e.id),
		(SELECT count(*) FROM usage_records WHERE execution_id = e.id),
		(SELECT count(*) FROM owner_decisions WHERE execution_id = e.id),
		(SELECT count(*) FROM check_results WHERE execution_id = e.id)
		FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, id).
		Scan(&s.Execution, &s.Task, &s.StageRuns, &s.Events, &s.Audits, &s.Usage, &s.Decisions, &s.Results); err != nil {
		t.Fatal(err)
	}
	return s
}

// spec 04 3.4.1: a resend of the same report returns the saved response
// and adds no rows or events; different content for the same (stage,
// attempt) is 409. Parallel resends apply once.
func TestStageFinishedResends(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	attempt := runTo(t, f, token, id, "check")
	input := finished("check", attempt, programResult("check", "blocked", nil))
	data, _ := json.Marshal(input)
	results := make(chan string, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body := postReportRaw(t, f, token, id, string(data))
			results <- fmt.Sprintf("%d %s", status, strings.TrimSpace(string(body)))
		}()
	}
	wg.Wait()
	close(results)
	for r := range results {
		if r != `200 {"next":"stop"}` {
			t.Fatalf("parallel resend: %s", r)
		}
	}
	after := readReportSnapshot(t, f, id)
	if after.Execution != "blocked" || after.Decisions != 1 || after.Results != 1 || after.Usage != 2 {
		t.Fatalf("after=%+v", after)
	}
	// Key order and whitespace do not change the content.
	var reordered map[string]any
	_ = json.Unmarshal(data, &reordered)
	pretty, _ := json.MarshalIndent(reordered, "", "  ")
	status, body := postReportRaw(t, f, token, id, string(pretty))
	if status != 200 || strings.TrimSpace(string(body)) != `{"next":"stop"}` || readReportSnapshot(t, f, id) != after {
		t.Fatalf("reordered resend: %d %s", status, body)
	}
	input["usage"] = map[string]any{"wallTimeMs": 101, "inputTokens": nil, "outputTokens": nil, "costAmount": nil}
	status, body = postReport(t, f, token, id, input)
	assertAuthError(t, status, body, 409, Conflict)
	if readReportSnapshot(t, f, id) != after {
		t.Fatal("conflicting resend mutated DB")
	}
	// A next=publish response is returned again with its content id.
	f2, token2, id2, _ := reportFixture(t)
	attempt = runTo(t, f2, token2, id2, "check")
	input = finished("check", attempt, programResult("check", "succeeded", nil))
	_, first := finishStage(t, f2, token2, id2, input)
	_, second := finishStage(t, f2, token2, id2, input)
	if string(first) != string(second) || !strings.Contains(string(first), `"checkedContentId":"content-1"`) {
		t.Fatalf("first=%s second=%s", first, second)
	}
}

// spec 04 3.4.1: result accepts only the start and the end of a stage.
func TestStageReportValidation(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	attempt := startStage(t, f, token, id, "work")
	before := readReportSnapshot(t, f, id)
	valid := func() map[string]any { return finished("work", attempt, nil) }
	for name, input := range map[string]map[string]any{
		"no_type":      {"stage": "work"},
		"legacy":       {"fromStatus": "working", "status": "checking"},
		"other_type":   {"type": "worktree_ready", "stage": "work"},
		"pushed":       {"type": "pushed", "stage": "publish"},
		"pr_created":   {"type": "pr_created", "stage": "publish"},
		"bad_stage":    func() map[string]any { v := valid(); v["stage"] = "deploy"; return v }(),
		"bad_attempt":  func() map[string]any { v := valid(); v["attempt"] = 0; return v }(),
		"runner_error": func() map[string]any { v := valid(); v["runnerError"] = "unknown"; return v }(),
		"missing_usage": func() map[string]any {
			v := valid()
			delete(v, "usage")
			return v
		}(),
		"negative_usage": func() map[string]any {
			v := valid()
			v["usage"] = map[string]any{"wallTimeMs": -1}
			return v
		}(),
		"work_result": func() map[string]any {
			v := valid()
			v["result"] = programResult("check", "succeeded", nil)
			return v
		}(),
		"missing_work":  func() map[string]any { v := valid(); v["work"] = nil; return v }(),
		"unknown_field": func() map[string]any { v := valid(); v["prNumber"] = 1; return v }(),
		"unknown_artifact": func() map[string]any {
			v := valid()
			v["work"] = map[string]any{"sessionId": nil, "summaryArtifactId": ulid.Make().String(), "question": nil}
			return v
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			status, body := postReport(t, f, token, id, input)
			assertAuthError(t, status, body, 400, ValidationFailed)
			if after := readReportSnapshot(t, f, id); after != before {
				t.Fatalf("invalid report mutated DB: %+v", after)
			}
		})
	}
	// A report for an attempt that was not started is 409.
	status, body := postReport(t, f, token, id, finished("work", attempt+1, nil))
	assertAuthError(t, status, body, 409, Conflict)
}

func TestStageReportRequiresClaimOwnerAndRunnerAuthentication(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	other, err := runnerauth.Register(context.Background(), f.db, "other-runner", "Other", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	input := started("work", addArtifact(t, f, id, "work", "request_json", "request.json"))
	before := readReportSnapshot(t, f, id)
	status, body := postReport(t, f, other, id, input)
	assertAuthError(t, status, body, 403, Forbidden)
	if n := count(t, f, `SELECT count(*) FROM audit_logs WHERE action = 'auth.reject'
		AND json_extract(detail, '$.reason') = 'execution_runner_mismatch'`); n != 1 {
		t.Fatalf("ownership denials=%d", n)
	}
	status, body = postReport(t, f, "invalid", id, input)
	assertAuthError(t, status, body, 401, Unauthenticated)
	status, body = f.request(t, "POST", "/api/executions/"+id+"/result", true, f.csrf, nil)
	assertAuthError(t, status, body, 403, Forbidden)
	if after := readReportSnapshot(t, f, id); after != before {
		t.Fatalf("denial mutated execution: %+v", after)
	}
	status, body = postReport(t, f, token, ulid.Make().String(), input)
	assertAuthError(t, status, body, 404, NotFound)
}

// A report for an Execution that is no longer the latest is 409.
func TestStageReportRejectsHistoricalExecution(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	execSQL(t, f, `INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		SELECT ?, task_id, seq+1, 'continue', 'pending', created_at FROM executions WHERE id = ?`, ulid.Make().String(), id)
	before := readReportSnapshot(t, f, id)
	status, body := postReport(t, f, token, id, started("work", addArtifact(t, f, id, "work", "request_json", "request.json")))
	assertAuthError(t, status, body, 409, Conflict)
	if after := readReportSnapshot(t, f, id); after != before {
		t.Fatalf("historical execution changed: %+v", after)
	}
}

// Every write of a stage end shares one transaction.
func TestStageFinishedRollsBackAllWritesOnStorageFailure(t *testing.T) {
	for i, trigger := range []string{
		`BEFORE INSERT ON usage_records`,
		`BEFORE UPDATE OF status ON tasks`,
		`BEFORE INSERT ON owner_decisions`,
		`BEFORE INSERT ON check_results`,
		`BEFORE INSERT ON notification_requests`,
		`BEFORE INSERT ON execution_events WHEN NEW.type = 'stage_finished'`,
		`BEFORE INSERT ON audit_logs WHEN NEW.action = 'execution.result'`,
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f, token, id, _ := reportFixture(t)
			attempt := runTo(t, f, token, id, "check")
			before := readReportSnapshot(t, f, id)
			execSQL(t, f, `CREATE TRIGGER reject_result `+trigger+` BEGIN SELECT RAISE(ABORT, 'storage unavailable'); END`)
			status, body := postReport(t, f, token, id, finished("check", attempt, programResult("check", "blocked", nil)))
			assertAuthError(t, status, body, 500, Internal)
			if after := readReportSnapshot(t, f, id); after != before {
				t.Fatalf("partial result survived failure: before=%+v after=%+v", before, after)
			}
			if run, _ := stageRunStatus(t, f, id, "check"); run != "running" {
				t.Fatalf("stage run=%s", run)
			}
		})
	}
}
