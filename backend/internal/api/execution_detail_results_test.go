package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/oklog/ulid/v2"
)

// Upload, report and read through the production router with a real owner
// session and Runner token. No response types are used to decode the detail.
func TestExecutionDetailResultsHTTPIntegration(t *testing.T) {
	f, token, _ := artifactFixture(t)
	id := otherExecution // The latest Execution of task-001.
	execSQL(t, f, `UPDATE tasks SET status='running' WHERE id='task-001'`)
	execSQL(t, f, `UPDATE executions SET status='claimed', runner_id='mac-mini', route_kind='project',
		project_id='project-a', harness_id='fixture-harness', profile_id='fixture-profile',
		contract_version='1', params='{}' WHERE id=?`, id)

	upload := func(stage, kind, name, content string) string {
		t.Helper()
		artifact := ulid.Make().String()
		path := fmt.Sprintf("/api/executions/%s/artifacts?artifactId=%s&stage=%s&kind=%s&name=%s&redacted=1", id, artifact, stage, kind, name)
		status, body := postArtifact(t, f, token, path, content)
		if status != http.StatusNoContent {
			t.Fatalf("upload %s: %d %s", name, status, body)
		}
		return artifact
	}
	var wantArtifacts []any
	var wantStages []any
	findings := []any{map[string]any{
		"rule": "review", "severity": "info", "message": "確認済み",
		"location": map[string]any{"path": "report.md", "line": float64(2)},
	}}
	outputs := []any{map[string]any{"kind": "document", "label": "報告書", "url": "https://example.invalid/report"}}
	var wantCheckID string
	for _, stage := range []string{"work", "check", "publish"} {
		requestID := upload(stage, "request_json", "request.json", `{}`)
		wantArtifacts = append(wantArtifacts, map[string]any{"id": requestID, "kind": "request_json", "stage": stage, "name": "request.json"})
		status, body := postReport(t, f, token, id, started(stage, requestID))
		if status != 200 {
			t.Fatalf("start %s: %d %s", stage, status, body)
		}
		var attempt struct{ Attempt int }
		if err := json.Unmarshal(body, &attempt); err != nil {
			t.Fatal(err)
		}
		input := finished(stage, attempt.Attempt, nil)
		var message, summary any
		if stage != "work" {
			message, summary = stage+" message", stage+" summary"
			extra := map[string]any{"message": message, "summary": summary}
			if stage == "check" {
				extra["findings"] = findings
			} else {
				extra["outputs"] = outputs
			}
			result := programResult(stage, "succeeded", extra)
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			resultID := upload(stage, "result_json", "result.json", string(data))
			input["result"], input["resultArtifactId"] = result, resultID
			wantArtifacts = append(wantArtifacts, map[string]any{"id": resultID, "kind": "result_json", "stage": stage, "name": "result.json"})
		}
		finishStage(t, f, token, id, input)
		var runID, startedAt, finishedAt string
		if err := f.db.QueryRow(`SELECT id, started_at, finished_at FROM stage_runs WHERE execution_id=? AND stage=?`, id, stage).
			Scan(&runID, &startedAt, &finishedAt); err != nil {
			t.Fatal(err)
		}
		wantStages = append(wantStages, map[string]any{
			"id": runID, "stage": stage, "attempt": float64(1), "status": "succeeded", "errorClass": nil,
			"startedAt": startedAt, "finishedAt": finishedAt, "message": message, "summary": summary,
		})
		if stage == "check" {
			if err := f.db.QueryRow(`SELECT id FROM check_results WHERE execution_id=?`, id).Scan(&wantCheckID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The list covers non-result files too, but exposes no storage paths or bodies.
	for _, item := range []struct{ stage, kind, name string }{
		{"work", "out_file", "report.md"}, {"work", "work_summary", "summary.md"},
		{"work", "question", "question.json"}, {"runner", "log", "runner.log"},
	} {
		artifact := upload(item.stage, item.kind, item.name, "private body")
		wantArtifacts = append(wantArtifacts, map[string]any{"id": artifact, "kind": item.kind, "stage": item.stage, "name": item.name})
	}
	// Other Executions, including earlier attempts of the same task, stay separate.
	uploadDownloadArtifact(t, f, token, "other Execution file")
	execSQL(t, f, `INSERT INTO stage_runs(id,execution_id,stage,attempt,status,started_at)
		VALUES ('other-check',?,'check',1,'running','2026-11-10T00:00:00Z')`, streamExecution)
	execSQL(t, f, `INSERT INTO check_results(id,execution_id,stage_run_id,result,content_id,summary,findings,created_at)
		VALUES ('other-result',?,'other-check','fail','other-content','other-summary','[]','2026-11-10T00:00:00Z')`, streamExecution)
	requireDetailFields(t, requireExecutionDetail(t, f, id), map[string]any{
		"status": "succeeded", "params": map[string]any{}, "contractVersion": "1", "resumeStage": nil,
		"stageRuns": wantStages, "artifacts": wantArtifacts, "outputs": outputs,
		"checkResults": []any{map[string]any{"id": wantCheckID, "result": "pass", "contentId": "content-1", "summary": "check summary", "findings": findings}},
	})
	history := requireTaskDetail(t, f, "task-001")["executions"].([]any)
	requireDetailFields(t, history[0].(map[string]any), map[string]any{"outputs": []any{}})
	requireDetailFields(t, history[1].(map[string]any), map[string]any{"outputs": outputs})
}

func TestExecutionDetailRetainsResumedAttemptsAndCheckHistory(t *testing.T) {
	f := taskFixture(t)
	id := pendingDetailExecution(t, f)
	execSQL(t, f, `UPDATE executions SET resume_stage='publish' WHERE id=?`, id)
	// Insert in reverse order to check chronology independent of insertion order.
	for i, status := range []string{"running", "blocked", "failed"} {
		attempt := 3 - i
		runID := fmt.Sprintf("run-%d", attempt)
		var finishedAt, errorClass any
		if status != "running" {
			finishedAt = fmt.Sprintf("2026-11-10T00:0%d:30Z", attempt)
		}
		if status == "failed" {
			errorClass = "check_failed"
		}
		execSQL(t, f, `INSERT INTO stage_runs(id,execution_id,stage,attempt,status,error_class,started_at,finished_at)
			VALUES (?,?,'check',?,?,?,?,?)`, runID, id, attempt, status, errorClass,
			fmt.Sprintf("2026-11-10T00:0%d:00.123456+00:00", attempt), finishedAt)
		if status != "running" {
			result := map[string]string{"failed": "fail", "blocked": "blocked"}[status]
			execSQL(t, f, `INSERT INTO check_results(id,execution_id,stage_run_id,result,findings,created_at)
				VALUES (?,?,?,?,'[]',?)`, "check-"+runID, id, runID, result, fmt.Sprintf("2026-11-10T00:0%d:30Z", attempt))
		}
	}
	detail := requireExecutionDetail(t, f, id)
	requireDetailFields(t, detail, map[string]any{"resumeStage": "publish"})
	runs := detail["stageRuns"].([]any)
	if len(runs) != 3 {
		t.Fatalf("stageRuns=%v", runs)
	}
	for i, status := range []string{"failed", "blocked", "running"} {
		var finishedAt, errorClass any
		if i < 2 {
			finishedAt = fmt.Sprintf("2026-11-10T00:0%d:30Z", i+1)
		}
		if i == 0 {
			errorClass = "check_failed"
		}
		requireDetailFields(t, runs[i].(map[string]any), map[string]any{
			"stage": "check", "attempt": float64(i + 1), "status": status, "errorClass": errorClass,
			"startedAt": fmt.Sprintf("2026-11-10T00:0%d:00.123456Z", i+1), "finishedAt": finishedAt,
			"message": nil, "summary": nil,
		})
	}
	requireDetailFields(t, detail, map[string]any{"checkResults": []any{
		map[string]any{"id": "check-run-1", "result": "fail", "contentId": nil, "summary": nil, "findings": []any{}},
		map[string]any{"id": "check-run-2", "result": "blocked", "contentId": nil, "summary": nil, "findings": []any{}},
	}})
}

func TestExecutionDetailUnreadableResultUsesCommonError(t *testing.T) {
	f, token, _ := artifactFixture(t)
	uploadDownloadArtifact(t, f, token, "not JSON")
	execSQL(t, f, `UPDATE artifacts SET kind='result_json', stage='check', storage_path='../outside' WHERE id=?`, artifactID)
	execSQL(t, f, `INSERT INTO stage_runs(id,execution_id,stage,attempt,status,result_artifact_id,started_at)
		VALUES ('broken',?,'check',1,'failed',?,'2026-11-10T00:00:00Z')`, streamExecution, artifactID)
	status, body := f.request(t, "GET", "/api/executions/"+streamExecution, true, "", nil)
	assertAuthError(t, status, body, 500, Internal)
	if bytes.Contains(body, []byte("outside")) {
		t.Fatalf("storage path leaked: %s", body)
	}
}

func TestExecutionDetailInvalidResultJSONRemainsReadable(t *testing.T) {
	f, token, _ := artifactFixture(t)
	uploadDownloadArtifact(t, f, token, `{"message":"partial", "summary":`)
	execSQL(t, f, `UPDATE artifacts SET kind='result_json', stage='check' WHERE id=?`, artifactID)
	execSQL(t, f, `INSERT INTO stage_runs(id,execution_id,stage,attempt,status,error_class,result_artifact_id,started_at)
		VALUES ('invalid',?,'check',1,'failed','contract_error',?,'2026-11-10T00:00:00Z')`, streamExecution, artifactID)
	detail := requireExecutionDetail(t, f, streamExecution)
	run := detail["stageRuns"].([]any)[0].(map[string]any)
	requireDetailFields(t, run, map[string]any{"status": "failed", "errorClass": "contract_error", "message": nil, "summary": nil})
}
