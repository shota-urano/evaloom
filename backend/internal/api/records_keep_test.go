package api

import (
	"testing"
)

// Spec 19 3.8 (save) and contract 9: only an Execution that succeeded through
// the publish stage keeps its records.keep files, and the work stage's output
// is never a record.

const keepAllStages = `{"keep":["check/report.md","publish/report.md","work/report.md"],"pass":{"recent":3}}`

func TestRecordsKeptOnlyWhenPublishSucceeded(t *testing.T) {
	for _, tc := range []struct {
		name, stage, status, errorClass string
		execution, task, stateClass     string
	}{
		{"check_failed", "check", "failed", "check_failed", "failed", "failed", "check_failed"},
		{"check_blocked", "check", "blocked", "", "blocked", "blocked", ""},
		{"publish_failed", "publish", "failed", "publish_failed", "failed", "failed", "publish_failed"},
		{"publish_unknown", "publish", "unknown", "publish_unknown", "blocked", "blocked", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, id, _ := reportFixture(t)
			setDefinition(t, f, id, "$.records", keepAllStages)
			attempt := runTo(t, f, token, id, tc.stage)
			addArtifact(t, f, id, "work", "out_file", "report.md")
			addArtifact(t, f, id, "check", "out_file", "report.md")
			if tc.stage == "publish" {
				addArtifact(t, f, id, "publish", "out_file", "report.md")
			}
			extra := map[string]any{}
			if tc.errorClass != "" {
				extra["errorClass"] = tc.errorClass
			}
			if tc.status == "blocked" {
				extra["findings"] = []any{map[string]any{"rule": "spec_mismatch", "severity": "error", "message": "differs"}}
			}
			finishStage(t, f, token, id, finished(tc.stage, attempt, programResult(tc.stage, tc.status, extra)))
			requireState(t, f, id, tc.execution, tc.task, tc.stateClass)
			if n := count(t, f, `SELECT count(*) FROM harness_records`); n != 0 {
				t.Fatalf("records=%d; an Execution that did not succeed through publish keeps none", n)
			}
		})
	}
}

func TestRecordsKeepOnlyFixedProgramOutputs(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	// Registration rejects "work/..." (records.keep[0] invalid_format); a stored
	// definition that still names it must not turn work output into a record.
	setDefinition(t, f, id, "$.records", keepAllStages)
	attempt := runTo(t, f, token, id, "publish")
	work := addArtifact(t, f, id, "work", "out_file", "report.md")
	check := addArtifact(t, f, id, "check", "out_file", "report.md")
	publish := addArtifact(t, f, id, "publish", "out_file", "report.md")
	finishStage(t, f, token, id, finished("publish", attempt, programResult("publish", "succeeded", map[string]any{"outputs": []any{}})))
	requireState(t, f, id, "succeeded", "completed", "")
	got := map[string]bool{}
	rows, err := f.db.Query(`SELECT artifact_id FROM harness_records WHERE execution_id = ? AND harness_id = 'harness-a'`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var artifact string
		if err := rows.Scan(&artifact); err != nil {
			t.Fatal(err)
		}
		got[artifact] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[check] || !got[publish] || got[work] {
		t.Fatalf("records=%v; want check %s and publish %s, not work %s", got, check, publish, work)
	}
}
