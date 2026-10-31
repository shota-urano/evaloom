package api

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

func verifyResultFixture(t *testing.T) (*authFixture, string, string, map[string]any) {
	t.Helper()
	f, token, id, _ := resultFixture(t, "verifying")
	if _, err := f.db.Exec(`UPDATE projects SET test_command='make test'`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := withGateSHAs(resultBody("verifying", "publishing"))
	input["verifiedSha"] = strings.Repeat("c", 40)
	input["gateResults"] = []any{gateReport(2, "pass")}
	input["testRuns"] = []model.TestRun{{ID: ulid.Make().String(), ExecutionID: id, Kind: "verify", TargetSHA: strings.Repeat("c", 40), Command: "make test", ExitCode: new(0), Total: new(2), Passed: new(2), Failed: new(0), Skipped: new(0), StartedAt: now, FinishedAt: now}}
	return f, token, id, input
}

func TestResultVerifyRejectsForgedMeasurementsAtomically(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		mutate func(map[string]any, *model.TestRun)
	}{
		{"WrongCommit", 422, func(_ map[string]any, r *model.TestRun) { r.TargetSHA = strings.Repeat("d", 40) }},
		{"WrongCommand", 400, func(_ map[string]any, r *model.TestRun) { r.Command = "other test" }},
		{"ForeignExecution", 400, func(_ map[string]any, r *model.TestRun) { r.ExecutionID = ulid.Make().String() }},
		{"ForeignArtifact", 422, func(_ map[string]any, r *model.TestRun) { r.TestIDsArtifactID = new(ulid.Make().String()) }},
		{"InconsistentCounts", 400, func(_ map[string]any, r *model.TestRun) { r.Skipped = new(1) }},
		{"FailingTestCannotPass", 400, func(_ map[string]any, r *model.TestRun) { r.ExitCode = new(1) }},
		{"WrongVerifiedSHA", 400, func(in map[string]any, _ *model.TestRun) { in["verifiedSha"] = strings.Repeat("d", 40) }},
		{"WrongKind", 400, func(_ map[string]any, r *model.TestRun) { r.Kind = "baseline" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, id, input := verifyResultFixture(t)
			before := readResultSnapshot(t, f, id)
			runs := input["testRuns"].([]model.TestRun)
			tc.mutate(input, &runs[0])
			if status, body := postResult(t, f, token, id, input); status != tc.status {
				t.Fatalf("status=%d body=%s", status, body)
			}
			if after := readResultSnapshot(t, f, id); !reflect.DeepEqual(before, after) {
				t.Fatalf("rejection changed state: %+v -> %+v", before, after)
			}
			var n int
			if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM test_runs)+(SELECT count(*) FROM gate_results)`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("partial records=%d %v", n, err)
			}
		})
	}
}

func TestResultVerifyAndGateRollbackTogether(t *testing.T) {
	f, token, id, input := verifyResultFixture(t)
	before := readResultSnapshot(t, f, id)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_gate BEFORE INSERT ON gate_results BEGIN SELECT RAISE(ABORT,'fixture gate write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if status, body := postResult(t, f, token, id, input); status != 500 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if after := readResultSnapshot(t, f, id); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed transaction changed state: %+v -> %+v", before, after)
	}
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM test_runs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("orphaned verify runs=%d %v", n, err)
	}
}
