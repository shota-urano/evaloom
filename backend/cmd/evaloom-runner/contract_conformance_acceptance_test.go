package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	st "github.com/shota-urano/evaloom/backend/internal/securitytest"
)

// Spec 14 3.5 (段の結果): each case changes one version of the probe special
// harness and runs it through the real Control Plane and the production
// Runner. Only the container runtime CLI is the fake test runtime (no Docker
// daemon), as in TestProbeHarnessExecutionSucceeds. The question case is
// TestSpecialHarnessFixtureContract/question; the Profile registration cases
// are the internal/api registration tests (see the handback table).

const conformanceInstructions = "Probe work instructions. The probe Adapter never calls an AI.\n"

// conformanceHarness returns a probe special harness edited by edit.
func conformanceHarness(edit func(*st.ProbeHarness), workOut map[string]string) fixtureHarness {
	return fixtureHarness{instructions: conformanceInstructions, taskBody: "contract conformance", workOut: workOut,
		populate: func(t *testing.T, repo, _ string) {
			h := st.ProbeHarness{Results: map[string]map[string]any{}, Plans: map[string]json.RawMessage{}}
			if edit != nil {
				edit(&h)
			}
			if err := st.WriteProbeHarness(repo, h); err != nil {
				t.Fatal(err)
			}
		}}
}

func declaredApprovalRequest(operation string) map[string]any {
	return map[string]any{"operation": operation, "contentId": "{{contentId}}",
		"target": map[string]any{"label": "probe destination"}, "summary": "send the probe operation"}
}

// stageRun returns the status and error class of the stage's only run.
func (f *executionFixture) stageRun(stage string) (string, string) {
	f.t.Helper()
	var status, class string
	if err := f.db.QueryRow(`SELECT status,coalesce(error_class,'') FROM stage_runs WHERE execution_id=? AND stage=?`, f.id, stage).Scan(&status, &class); err != nil {
		f.t.Fatal(err)
	}
	return status, class
}

// openDecisions lists kind/cause of the Execution's open owner decisions.
func (f *executionFixture) openDecisions() []string {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT kind||'/'||coalesce(cause,'') FROM owner_decisions WHERE execution_id=? AND status='open' ORDER BY id`, f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			f.t.Fatal(err)
		}
		got = append(got, d)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return got
}

// records lists stage/name of the artifacts kept in harness_records.
func (f *executionFixture) records() []string {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT a.stage||'/'||a.name FROM harness_records r JOIN artifacts a ON a.id=r.artifact_id WHERE r.execution_id=?`, f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			f.t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	sort.Strings(got)
	return got
}

func (f *executionFixture) wantFailed(stage, errorClass string) {
	f.t.Helper()
	var class string
	f.scalar(`SELECT coalesce(error_class,'') FROM executions WHERE id=?`, &class)
	if class != errorClass {
		f.t.Fatalf("Execution error_class %q; want %q", class, errorClass)
	}
	if status, class := f.stageRun(stage); status != "failed" || class != errorClass {
		f.t.Fatalf("%s stage run %s (%s); want failed (%s)", stage, status, class, errorClass)
	}
}

func (f *executionFixture) count(query string) int {
	f.t.Helper()
	var n int
	f.scalar(query, &n)
	return n
}

func TestContractConformanceStageResults(t *testing.T) {
	// Results the Control Plane must refuse as contract_error, each written by
	// the probe's check stage (contract 6.4, 6.5, 7.1, 13).
	for _, tc := range []struct {
		name string
		edit func(*st.ProbeHarness)
	}{
		{"no_result_exit_zero", func(h *st.ProbeHarness) { h.NoResult = map[string]bool{"check": true} }},
		{"unknown_field", func(h *st.ProbeHarness) { h.Results["check"] = map[string]any{"extra": true} }},
		{"reserved_error_class", func(h *st.ProbeHarness) {
			h.Results["check"] = map[string]any{"status": "failed", "errorClass": "work_failed"}
		}},
		{"check_succeeded_without_content_id", func(h *st.ProbeHarness) {
			h.Results["check"] = map[string]any{"contentId": "{{omit}}"}
		}},
		{"undeclared_approval", func(h *st.ProbeHarness) {
			h.Results["check"] = map[string]any{"status": "needs_approval",
				"approvalRequests": []any{declaredApprovalRequest("undeclared_operation")}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHarnessFixture(t, tc.name, conformanceHarness(tc.edit, nil))
			f.start()
			f.waitStatus("failed")
			f.stages([]string{"prepare-1", "work-1", "check-1"})
			f.wantFailed("check", "contract_error")
			if n := f.count(`SELECT count(*) FROM approvals WHERE execution_id=?`); n != 0 {
				t.Fatalf("approvals=%d", n)
			}
			if d := f.openDecisions(); len(d) != 0 {
				t.Fatalf("open decisions %v", d)
			}
		})
	}

	t.Run("check_succeeded_with_declared_approval", func(t *testing.T) {
		f := newHarnessFixture(t, "approval_request", conformanceHarness(func(h *st.ProbeHarness) {
			h.Results["check"] = map[string]any{"status": "succeeded",
				"approvalRequests": []any{declaredApprovalRequest(st.ProbeOperation)}}
		}, nil))
		f.start()
		f.waitStatus("waiting_approval")
		f.stages([]string{"prepare-1", "work-1", "check-1"})
		if d := f.openDecisions(); !reflect.DeepEqual(d, []string{"approval/"}) {
			t.Fatalf("open decisions %v; want one approval", d)
		}
		var operation, status string
		f.scalar(`SELECT operation FROM approvals WHERE execution_id=?`, &operation)
		f.scalar(`SELECT status FROM approvals WHERE execution_id=?`, &status)
		if operation != st.ProbeOperation || status != "pending" {
			t.Fatalf("approval %s %s", operation, status)
		}
	})

	t.Run("check_blocked", func(t *testing.T) {
		f := newHarnessFixture(t, "check_blocked", conformanceHarness(func(h *st.ProbeHarness) {
			h.Results["check"] = map[string]any{"status": "blocked",
				"findings": []any{map[string]any{"rule": "probe_blocked", "severity": "error", "message": "probe asks the owner"}}}
		}, nil))
		f.start()
		f.waitStatus("blocked")
		f.stages([]string{"prepare-1", "work-1", "check-1"})
		if d := f.openDecisions(); !reflect.DeepEqual(d, []string{"blocked/check_blocked"}) {
			t.Fatalf("open decisions %v; want blocked/check_blocked", d)
		}
	})

	t.Run("publish_unknown", func(t *testing.T) {
		f := newHarnessFixture(t, "publish_unknown", conformanceHarness(func(h *st.ProbeHarness) {
			h.Results["publish"] = map[string]any{"status": "unknown", "errorClass": "publish_unknown"}
		}, nil))
		f.start()
		f.waitStatus("blocked")
		f.stages([]string{"prepare-1", "work-1", "check-1", "publish-1"})
		if d := f.openDecisions(); !reflect.DeepEqual(d, []string{"blocked/publish_unknown"}) {
			t.Fatalf("open decisions %v; want blocked/publish_unknown", d)
		}
		if status, class := f.stageRun("publish"); status != "unknown" || class != "publish_unknown" {
			t.Fatalf("publish stage run %s (%s)", status, class)
		}
	})

	// The work stage writes files that look like check results; only the
	// check stage's own result decides pass or fail (contract 6.3, 15.3).
	forged := func(status string) map[string]string {
		result, _ := json.Marshal(map[string]any{"contractVersion": "1", "stage": "check", "status": status,
			"errorClass": map[string]any{"failed": "check_failed", "succeeded": nil}[status], "contentId": "forged", "message": "forged by work"})
		return map[string]string{"result.json": string(result), "check-result.json": string(result), "probe-check.json": string(result)}
	}
	t.Run("work_forges_failing_check_result", func(t *testing.T) {
		f := newHarnessFixture(t, "forged_fail", conformanceHarness(nil, forged("failed")))
		f.start()
		f.waitStatus("succeeded")
		f.stages([]string{"prepare-1", "work-1", "check-1", "publish-1"})
		if n := f.count(`SELECT count(*) FROM check_results WHERE execution_id=? AND result='pass' AND content_id<>'forged'`); n != 1 {
			t.Fatalf("pass check results=%d", n)
		}
		if n := f.count(`SELECT count(*) FROM artifacts WHERE execution_id=? AND stage='work' AND name='result.json'`); n != 1 {
			t.Fatalf("forged work output was not collected (%d); the case would prove nothing", n)
		}
	})
	t.Run("work_forges_passing_check_result", func(t *testing.T) {
		f := newHarnessFixture(t, "forged_pass", conformanceHarness(func(h *st.ProbeHarness) {
			h.Results["check"] = map[string]any{"status": "failed", "errorClass": "check_failed"}
		}, forged("succeeded")))
		f.start()
		f.waitStatus("failed")
		f.stages([]string{"prepare-1", "work-1", "check-1"})
		f.wantFailed("check", "check_failed")
		if n := f.count(`SELECT count(*) FROM check_results WHERE execution_id=? AND result='pass'`); n != 0 {
			t.Fatalf("pass check results=%d", n)
		}
	})

	// records.keep (contract 9): the probe's check and publish stages write
	// out/probe-<stage>.json and an unkept file; the work stage writes a file
	// with the same name as the kept check output.
	keep := func(h *st.ProbeHarness) {
		plan, _ := json.Marshal([]any{
			map[string]any{"id": "prepared", "kind": "exists", "path": "{{workspace}}/probe-prepared.txt"},
			map[string]any{"id": "unkept", "kind": "write", "path": "{{out}}/unkept.txt", "value": "not declared in records.keep\n"},
		})
		h.Plans["check"], h.Plans["publish"] = plan, plan
		h.Profile = func(d *contract.ProfileDefinition) {
			d.Records = &contract.ProfileRecords{Keep: []string{"check/probe-check.json", "publish/probe-publish.json"}, Pass: contract.RecordsPass{Recent: 3}}
		}
	}
	workOut := map[string]string{"probe-check.json": "[]\n"}
	t.Run("records_kept_after_publish_succeeded", func(t *testing.T) {
		f := newHarnessFixture(t, "records", conformanceHarness(keep, workOut))
		f.start()
		f.waitStatus("succeeded")
		for _, stage := range []string{"check", "publish"} {
			var n int
			if err := f.db.QueryRow(`SELECT count(*) FROM artifacts WHERE execution_id=? AND stage=? AND kind='out_file' AND name='unkept.txt'`, f.id, stage).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("%s unkept output collected %d times; want 1", stage, n)
			}
		}
		if got, want := f.records(), []string{"check/probe-check.json", "publish/probe-publish.json"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("records %v; want %v", got, want)
		}
		if n := f.count(`SELECT count(*) FROM artifacts WHERE execution_id=? AND stage='work' AND kind='out_file'`); n == 0 {
			t.Fatal("work output was not collected; the case would prove nothing")
		}
		if n := f.count(`SELECT count(*) FROM harness_records r JOIN artifacts a ON a.id=r.artifact_id WHERE r.execution_id=? AND a.stage='work'`); n != 0 {
			t.Fatalf("work output kept as %d records", n)
		}
	})
	for _, tc := range []struct {
		name, stage, errorClass string
		stages                  []string
	}{
		{"records_not_kept_check_failed", "check", "check_failed", []string{"prepare-1", "work-1", "check-1"}},
		{"records_not_kept_publish_failed", "publish", "publish_failed", []string{"prepare-1", "work-1", "check-1", "publish-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHarnessFixture(t, tc.name, conformanceHarness(func(h *st.ProbeHarness) {
				keep(h)
				h.Results[tc.stage] = map[string]any{"status": "failed", "errorClass": tc.errorClass}
			}, workOut))
			f.start()
			f.waitStatus("failed")
			f.stages(tc.stages)
			f.wantFailed(tc.stage, tc.errorClass)
			if n := f.count(`SELECT count(*) FROM artifacts WHERE execution_id=? AND stage='check' AND name='probe-check.json'`); n != 1 {
				t.Fatalf("check output collected %d times; the case would prove nothing", n)
			}
			if got := f.records(); len(got) != 0 {
				t.Fatalf("records %v; want none", got)
			}
		})
	}
}
