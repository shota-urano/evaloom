package api

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

func baselineReport(t *testing.T) (*authFixture, string, string, map[string]any) {
	t.Helper()
	f, token, id, worktree := resultFixture(t, "preparing")
	if _, err := f.db.Exec(`UPDATE projects SET test_command='make test' WHERE id='project-a'`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := resultBody("preparing", "preparing")
	input["worktreeId"], input["baseSha"] = worktree, strings.Repeat("a", 40)
	input["baseline"] = &model.TestRun{ID: ulid.Make().String(), ExecutionID: id, Kind: "baseline", TargetSHA: strings.Repeat("a", 40), Command: "make test",
		ExitCode: new(0), Total: new(3), Passed: new(2), Failed: new(0), Skipped: new(1), StartedAt: now, FinishedAt: now}
	return f, token, id, input
}

func TestResultBaselineMeasurementIsIdempotentWithoutStateTransition(t *testing.T) {
	f, token, id, input := baselineReport(t)
	before := readResultSnapshot(t, f, id)
	for range 2 {
		if status, body := postResult(t, f, token, id, input); status != 204 {
			t.Fatalf("baseline status=%d body=%s", status, body)
		}
	}
	if after := readResultSnapshot(t, f, id); !reflect.DeepEqual(before, after) {
		t.Fatalf("measurement transitioned state or emitted side effects: before=%+v after=%+v", before, after)
	}
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM test_runs WHERE kind='baseline'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
	input["baseline"].(*model.TestRun).ID = ulid.Make().String()
	if status, body := postResult(t, f, token, id, input); status != 409 {
		t.Fatalf("replacement status=%d body=%s", status, body)
	}
}

func TestResultBaselineRejectsInvalidMeasurementsWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		change func(map[string]any, *model.TestRun)
	}{
		{"wrong execution", 400, func(_ map[string]any, run *model.TestRun) { run.ExecutionID = ulid.Make().String() }},
		{"wrong target", 400, func(_ map[string]any, run *model.TestRun) { run.TargetSHA = strings.Repeat("b", 40) }},
		{"worktree start mismatch", 422, func(in map[string]any, run *model.TestRun) {
			in["baseSha"] = strings.Repeat("b", 40)
			run.TargetSHA = strings.Repeat("b", 40)
		}},
		{"wrong kind", 400, func(_ map[string]any, run *model.TestRun) { run.Kind = "verify" }},
		{"invalid id", 400, func(_ map[string]any, run *model.TestRun) { run.ID = "bad" }},
		{"wrong command", 400, func(_ map[string]any, run *model.TestRun) { run.Command = "other test" }},
		{"negative count", 400, func(_ map[string]any, run *model.TestRun) { run.Total = new(-1) }},
		{"inconsistent counts", 400, func(_ map[string]any, run *model.TestRun) { run.Total = new(1) }},
		{"invalid finish", 400, func(_ map[string]any, run *model.TestRun) { run.FinishedAt = run.StartedAt.Add(-time.Second) }},
		{"foreign artifact", 422, func(_ map[string]any, run *model.TestRun) { run.TestIDsArtifactID = new(ulid.Make().String()) }},
		{"unprepared worktree", 400, func(in map[string]any, _ *model.TestRun) { delete(in, "worktreeId") }},
		{"wrong stage", 409, func(in map[string]any, _ *model.TestRun) { in["status"] = "running" }},
		{"bundled gate", 400, func(in map[string]any, _ *model.TestRun) { in["gateResults"] = []any{gateReport(2, "pass")} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, id, input := baselineReport(t)
			before := readResultSnapshot(t, f, id)
			tc.change(input, input["baseline"].(*model.TestRun))
			if status, body := postResult(t, f, token, id, input); status != tc.status {
				t.Fatalf("status=%d body=%s; want %d", status, body, tc.status)
			}
			var rows int
			if err := f.db.QueryRow(`SELECT count(*) FROM test_runs`).Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("invalid report wrote measurement: rows=%d err=%v", rows, err)
			}
			if after := readResultSnapshot(t, f, id); !reflect.DeepEqual(before, after) {
				t.Fatalf("invalid report changed state: %+v %+v", before, after)
			}
			var base *string
			if err := f.db.QueryRow(`SELECT base_sha FROM executions WHERE id=?`, id).Scan(&base); err != nil || base != nil {
				t.Fatalf("invalid report associated start SHA: %v %v", base, err)
			}
		})
	}
}

func TestResultBaselineRollsBackAssociationOnStorageFailure(t *testing.T) {
	f, token, id, input := baselineReport(t)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_baseline BEFORE INSERT ON test_runs BEGIN SELECT RAISE(ABORT,'storage unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	before := readResultSnapshot(t, f, id)
	if status, body := postResult(t, f, token, id, input); status != 500 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if after := readResultSnapshot(t, f, id); !reflect.DeepEqual(before, after) {
		t.Fatalf("storage failure changed state: %+v %+v", before, after)
	}
	var base *string
	if err := f.db.QueryRow(`SELECT base_sha FROM executions WHERE id=?`, id).Scan(&base); err != nil || base != nil {
		t.Fatalf("failed report associated SHA: %v %v", base, err)
	}
}

func TestResultBaselineRequiresClaimOwner(t *testing.T) {
	f, token, id, input := baselineReport(t)
	if _, err := f.db.Exec(`UPDATE executions SET runner_id=NULL WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if status, body := postResult(t, f, token, id, input); status != 403 {
		t.Fatalf("unclaimed report status=%d body=%s", status, body)
	}
	var rows int
	if err := f.db.QueryRow(`SELECT count(*) FROM test_runs`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("unauthorized report wrote rows=%d err=%v", rows, err)
	}
}
