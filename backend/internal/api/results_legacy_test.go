package api

import (
	"database/sql"
	"testing"
)

// The helpers in this file let the tests of other features (notifications,
// cancellation, retry, recovery, approvals) bring an Execution to a state
// through the real stage reports of spec 04 3.4.1. The responses of the
// result API itself are tested in results_test.go.

var legacyStages = map[string]string{
	"preparing": "prepare", "working": "work", "checking": "check", "publishing": "publish",
}

// resultFixture claims a task and reports stages until the Execution is in
// from (claimed, preparing, working, checking or publishing). The stage of
// from is running.
func resultFixture(t *testing.T, from string) (*authFixture, string, string, string) {
	t.Helper()
	f, token, id, taskID := reportFixture(t)
	switch from {
	case "claimed":
	case "preparing":
		setDefinition(t, f, id, "$.stages.prepare", `{"command":["evaloom/prepare/run"],"timeoutSec":60}`)
		startStage(t, f, token, id, "prepare")
	case "working", "checking", "publishing":
		runTo(t, f, token, id, legacyStages[from])
	default:
		t.Fatalf("resultFixture: unsupported state %s", from)
	}
	return f, token, id, taskID
}

// resultBody describes a wanted Execution change from -> to. postResult
// turns it into stage reports.
func resultBody(from, to string) map[string]any {
	return map[string]any{"legacyFrom": from, "legacyTo": to}
}

// postResult sends a stage report. For a resultBody it ends the running
// stage (and starts the next stage for a progress state) and answers 204
// with an empty body when every report succeeded.
func postResult(t *testing.T, f *authFixture, token, id string, input any) (int, []byte) {
	t.Helper()
	m, ok := input.(map[string]any)
	if !ok || m["legacyTo"] == nil {
		return postReport(t, f, token, id, input)
	}
	to := m["legacyTo"].(string)
	// The latest stage run; a finished one lets a resend repeat its report.
	var stage, runStatus string
	var attempt int
	err := f.db.QueryRow(`SELECT stage, attempt, status FROM stage_runs WHERE execution_id = ?
		ORDER BY rowid DESC LIMIT 1`, id).Scan(&stage, &attempt, &runStatus)
	if err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if next, progress := legacyStages[to]; progress {
		if stage != "" && runStatus == "running" {
			if status, body := postReport(t, f, token, id, successReport(stage, attempt)); status != 200 {
				return status, body
			}
		}
		status, body := postReport(t, f, token, id, started(next, addArtifact(t, f, id, next, "request_json", "request.json")))
		if status != 200 {
			return status, body
		}
		return 204, nil
	}
	if stage == "" && to != "succeeded" {
		var status string
		if err := f.db.QueryRow(`SELECT status FROM executions WHERE id = ?`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "claimed" {
			stage = "work"
			attempt = startStage(t, f, token, id, stage)
		}
	}
	var report map[string]any
	switch to {
	case "succeeded":
		report = finished("publish", max(attempt, 1), programResult("publish", "succeeded", nil))
		if stage != "" && stage != "publish" {
			report["attempt"] = 99
		}
	case "failed", "timed_out", "cancelled":
		runnerError := map[string]string{"failed": "work_failed", "timed_out": "timeout", "cancelled": "cancelled"}[to]
		if stage == "" {
			stage, attempt = "work", 99
		}
		report = finished(stage, attempt, nil)
		report["runnerError"], report["exitCode"], report["work"] = runnerError, 1, nil
	default:
		// No stage report reaches this state: a report of an attempt that was
		// never started is a conflict.
		report = finished("publish", 99, programResult("publish", "succeeded", nil))
	}
	status, body := postReport(t, f, token, id, report)
	if status != 200 {
		return status, body
	}
	return 204, nil
}

func successReport(stage string, attempt int) map[string]any {
	if stage == "work" {
		return finished(stage, attempt, nil)
	}
	return finished(stage, attempt, programResult(stage, "succeeded", nil))
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
