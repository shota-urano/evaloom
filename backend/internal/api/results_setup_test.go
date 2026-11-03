package api

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
)

func setupReport(t *testing.T) (*authFixture, string, string, map[string]any) {
	t.Helper()
	f, token, id, worktree := resultFixture(t, "preparing")
	if _, err := f.db.Exec(`UPDATE projects SET setup_command='fixture setup' WHERE id='project-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET base_sha=? WHERE id=?`, strings.Repeat("a", 40), id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE worktrees SET setup_status='pending' WHERE id=?`, worktree); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := resultBody("preparing", "preparing")
	input["worktreeId"], input["setupStatus"] = worktree, "succeeded"
	input["testRuns"] = []model.TestRun{{ID: ulid.Make().String(), ExecutionID: id, Kind: "setup",
		TargetSHA: strings.Repeat("a", 40), Command: "fixture setup", ExitCode: new(0), StartedAt: now, FinishedAt: now}}
	return f, token, id, input
}

func TestResultSetupMeasurementIsIdempotentWithoutStateTransition(t *testing.T) {
	f, token, id, input := setupReport(t)
	before := readResultSnapshot(t, f, id)
	for range 2 {
		if status, body := postResult(t, f, token, id, input); status != 204 {
			t.Fatalf("status=%d body=%s", status, body)
		}
	}
	if after := readResultSnapshot(t, f, id); !reflect.DeepEqual(before, after) {
		t.Fatalf("measurement changed state: before=%+v after=%+v", before, after)
	}
	var count int
	var setup string
	if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM test_runs WHERE execution_id=?), w.setup_status
		FROM worktrees w JOIN executions e ON e.worktree_id=w.id WHERE e.id=?`, id, id).Scan(&count, &setup); err != nil || count != 1 || setup != "succeeded" {
		t.Fatalf("runs=%d setup=%s err=%v", count, setup, err)
	}
	input["testRuns"].([]model.TestRun)[0].ID = ulid.Make().String()
	if status, body := postResult(t, f, token, id, input); status != 409 {
		t.Fatalf("replacement status=%d body=%s", status, body)
	}
}

func TestResultSetupRejectsInvalidReportsWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		change func(map[string]any, *model.TestRun)
	}{
		{"wrong execution", 400, func(_ map[string]any, run *model.TestRun) { run.ExecutionID = ulid.Make().String() }},
		{"wrong worktree", 422, func(in map[string]any, _ *model.TestRun) { in["worktreeId"] = ulid.Make().String() }},
		{"wrong target", 422, func(_ map[string]any, run *model.TestRun) { run.TargetSHA = strings.Repeat("b", 40) }},
		{"wrong kind", 400, func(_ map[string]any, run *model.TestRun) { run.Kind = "verify" }},
		{"wrong command", 400, func(_ map[string]any, run *model.TestRun) { run.Command = "unregistered" }},
		{"missing status", 400, func(in map[string]any, _ *model.TestRun) { delete(in, "setupStatus") }},
		{"invalid status", 400, func(in map[string]any, _ *model.TestRun) { in["setupStatus"] = "pending" }},
		{"failed exit marked success", 400, func(_ map[string]any, run *model.TestRun) { run.ExitCode = new(23) }},
		{"timeout marked success", 400, func(_ map[string]any, run *model.TestRun) { run.TimedOut = true }},
		{"unknown exit marked success", 400, func(_ map[string]any, run *model.TestRun) { run.ExitCode = nil }},
		{"invalid times", 400, func(_ map[string]any, run *model.TestRun) { run.FinishedAt = run.StartedAt.Add(-time.Second) }},
		{"test counts", 400, func(_ map[string]any, run *model.TestRun) { run.Total = new(1) }},
		{"transition bundled", 409, func(in map[string]any, _ *model.TestRun) { in["status"] = "running" }},
		{"metadata bundled", 400, func(in map[string]any, _ *model.TestRun) { in["branchName"] = "other" }},
		{"multiple runs", 400, func(in map[string]any, run *model.TestRun) { in["testRuns"] = []model.TestRun{*run, *run} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, id, input := setupReport(t)
			runs := input["testRuns"].([]model.TestRun)
			tc.change(input, &runs[0])
			before := readResultSnapshot(t, f, id)
			if status, body := postResult(t, f, token, id, input); status != tc.status {
				t.Fatalf("status=%d want=%d body=%s", status, tc.status, body)
			}
			assertSetupNotSaved(t, f, id, before)
		})
	}
}

func assertSetupNotSaved(t *testing.T, f *authFixture, id string, before resultSnapshot) {
	t.Helper()
	if after := readResultSnapshot(t, f, id); !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected setup changed state: before=%+v after=%+v", before, after)
	}
	var count int
	var setup string
	if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM test_runs WHERE execution_id=?), w.setup_status
		FROM worktrees w JOIN executions e ON e.worktree_id=w.id WHERE e.id=?`, id, id).Scan(&count, &setup); err != nil || count != 0 || setup != "pending" {
		t.Fatalf("partial write: runs=%d setup=%s err=%v", count, setup, err)
	}
}

func TestResultSetupRequiresClaimingRunnerAuthentication(t *testing.T) {
	f, _, id, input := setupReport(t)
	other, err := runnerauth.Register(context.Background(), f.db, "other-setup-runner", "Other Runner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := readResultSnapshot(t, f, id)
	for _, tc := range []struct {
		token string
		want  int
	}{{"invalid-token", 401}, {other, 403}} {
		if status, body := postResult(t, f, tc.token, id, input); status != tc.want {
			t.Fatalf("status=%d want=%d body=%s", status, tc.want, body)
		}
		assertSetupNotSaved(t, f, id, before)
	}
}

func TestResultSetupRollsBackMeasurementIfStatusSaveFails(t *testing.T) {
	f, token, id, input := setupReport(t)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_setup BEFORE UPDATE OF setup_status ON worktrees
		BEGIN SELECT RAISE(ABORT, 'reject setup'); END`); err != nil {
		t.Fatal(err)
	}
	before := readResultSnapshot(t, f, id)
	if status, body := postResult(t, f, token, id, input); status != 500 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	assertSetupNotSaved(t, f, id, before)
}
