package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func gateReport(gate int, result string, findings ...store.Finding) map[string]any {
	if findings == nil {
		findings = []store.Finding{}
	}
	return map[string]any{"gate": gate, "result": result, "baseSha": strings.Repeat("a", 40), "targetSha": strings.Repeat("c", 40), "findings": findings}
}

// withGateSHAs records the SHAs gateReport uses in the same report.
func withGateSHAs(input map[string]any) map[string]any {
	input["harnessHeadSha"] = strings.Repeat("c", 40)
	return input
}

func TestResultSavesGate2FindingsAndLinksBlock(t *testing.T) {
	f, token, id, _ := resultFixture(t, "verifying")
	finding := store.Finding{Rule: "existing_test_not_run", Path: "src/foo.test.ts", Line: 0, Message: "既存のテスト FooTest.testBar が実行されていない"}
	input := withGateSHAs(resultBody("verifying", "blocked"))
	input["block"] = map[string]any{"cause": "gate2_missing_tests", "headSha": strings.Repeat("c", 40)}
	input["gateResults"] = []any{gateReport(2, "blocked", finding)}
	if status, body := postResult(t, f, token, id, input); status != 204 {
		t.Fatalf("%d %s", status, body)
	}
	var gateID, result, base, target, findings string
	var gate int
	if err := f.db.QueryRow(`SELECT id, gate, result, base_sha, target_sha, findings FROM gate_results WHERE execution_id = ?`, id).
		Scan(&gateID, &gate, &result, &base, &target, &findings); err != nil {
		t.Fatal(err)
	}
	var saved []store.Finding
	if err := json.Unmarshal([]byte(findings), &saved); err != nil || len(saved) != 1 || saved[0] != finding {
		t.Fatalf("findings=%s %v", findings, err)
	}
	if gate != 2 || result != "blocked" || base != strings.Repeat("a", 40) || target != strings.Repeat("c", 40) {
		t.Fatalf("gate=%d result=%s base=%s target=%s", gate, result, base, target)
	}
	var cause string
	var linked sql.NullString
	if err := f.db.QueryRow(`SELECT cause, gate_result_id FROM execution_blocks WHERE execution_id = ?`, id).Scan(&cause, &linked); err != nil {
		t.Fatal(err)
	}
	if cause != "gate2_missing_tests" || linked.String != gateID {
		t.Fatalf("cause=%s linked=%v gate=%s", cause, linked, gateID)
	}
	var payload string
	if err := f.db.QueryRow(`SELECT payload FROM execution_events WHERE execution_id = ? AND type = 'gate_result'`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != `{"findingCount":1,"gate":2,"result":"blocked"}` {
		t.Fatalf("payload=%s", payload)
	}
	if after := readResultSnapshot(t, f, id); after.Execution != "blocked" || after.Task != "blocked" ||
		after.Blocks != 1 || after.Requests != 1 || after.Worktree != "retained" {
		t.Fatalf("blocked state=%+v", after)
	}
}

func TestResultRejectsGate1WithoutWrites(t *testing.T) {
	for _, result := range []string{"pass", "blocked", "failed"} {
		t.Run(result, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "verifying")
			before := readResultSnapshot(t, f, id)
			target := result
			if result == "pass" {
				target = "publishing"
			}
			input := withGateSHAs(resultBody("verifying", target))
			input["gateResults"] = []any{gateReport(1, result)}
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 400, ValidationFailed)
			var gates int
			if err := f.db.QueryRow(`SELECT count(*) FROM gate_results WHERE execution_id = ?`, id).Scan(&gates); err != nil {
				t.Fatal(err)
			}
			if after := readResultSnapshot(t, f, id); after != before || gates != 0 {
				t.Fatalf("Gate 1 report wrote: %+v gates=%d", after, gates)
			}
		})
	}
}

func TestResultSavesPassingGatesWithNextTransition(t *testing.T) {
	f, token, id, _ := resultFixture(t, "verifying")
	input := withGateSHAs(resultBody("verifying", "publishing"))
	input["gateResults"] = []any{
		gateReport(2, "pass"), gateReport(3, "pass"),
	}
	if status, body := postResult(t, f, token, id, input); status != 204 {
		t.Fatalf("%d %s", status, body)
	}
	var count, events int
	if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM gate_results WHERE execution_id = ? AND result = 'pass'),
		(SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = 'gate_result')`, id, id).Scan(&count, &events); err != nil {
		t.Fatal(err)
	}
	if count != 2 || events != 2 {
		t.Fatalf("gate results=%d events=%d", count, events)
	}
}

func TestResultRejectsInvalidGateResultsWithoutWrites(t *testing.T) {
	for name, c := range map[string]struct {
		status string
		gates  []any
	}{
		"gate out of range":    {"publishing", []any{gateReport(4, "pass")}},
		"gate zero":            {"publishing", []any{gateReport(0, "pass")}},
		"unknown result":       {"publishing", []any{gateReport(2, "skipped")}},
		"blocked not stopping": {"publishing", []any{gateReport(2, "blocked")}},
		"failed while blocked": {"blocked", []any{gateReport(2, "failed")}},
		"short SHA":            {"publishing", []any{map[string]any{"gate": 2, "result": "pass", "baseSha": "abc", "targetSha": strings.Repeat("c", 40), "findings": []any{}}}},
		"empty rule":           {"publishing", []any{gateReport(2, "pass", store.Finding{Path: "x"})}},
		"negative line":        {"publishing", []any{gateReport(2, "pass", store.Finding{Rule: "existing_test_not_run", Line: -1})}},
	} {
		t.Run(name, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "verifying")
			before := readResultSnapshot(t, f, id)
			input := withGateSHAs(resultBody("verifying", c.status))
			input["gateResults"] = c.gates
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 400, ValidationFailed)
			var gates int
			if err := f.db.QueryRow(`SELECT count(*) FROM gate_results WHERE execution_id = ?`, id).Scan(&gates); err != nil {
				t.Fatal(err)
			}
			if after := readResultSnapshot(t, f, id); after != before || gates != 0 {
				t.Fatalf("rejected report wrote: %+v gates=%d", after, gates)
			}
		})
	}
}

func TestResultRejectsGateResultsForOtherCommits(t *testing.T) {
	other := map[string]any{"gate": 2, "result": "pass", "baseSha": strings.Repeat("d", 40), "targetSha": strings.Repeat("e", 40), "findings": []any{}}
	for name, input := range map[string]map[string]any{
		"SHAs not recorded": resultBody("verifying", "publishing"),
		"different commits": withGateSHAs(resultBody("verifying", "publishing")),
	} {
		t.Run(name, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "verifying")
			before := readResultSnapshot(t, f, id)
			input["gateResults"] = []any{other}
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 422, PreconditionFailed)
			if after := readResultSnapshot(t, f, id); after != before {
				t.Fatalf("rejected report wrote: %+v", after)
			}
		})
	}
	// SHAs saved by an earlier report are used.
	f, token, id, _ := resultFixture(t, "running")
	if status, body := postResult(t, f, token, id, withGateSHAs(resultBody("running", "verifying"))); status != 204 {
		t.Fatalf("%d %s", status, body)
	}
	input := resultBody("verifying", "publishing")
	input["gateResults"] = []any{gateReport(2, "pass")}
	if status, body := postResult(t, f, token, id, input); status != 204 {
		t.Fatalf("%d %s", status, body)
	}
}

func TestResultRequiresBlockCauseToMatchBlockingGate(t *testing.T) {
	block := func(cause string) map[string]any {
		return map[string]any{"cause": cause, "headSha": strings.Repeat("c", 40)}
	}
	for name, c := range map[string]struct {
		block map[string]any
		gates []any
	}{
		"Gate 3 result for a Gate 2 cause": {block("gate2_missing_tests"), []any{gateReport(3, "blocked")}},
		"two blocking Gates":               {block("gate2_missing_tests"), []any{gateReport(2, "blocked"), gateReport(3, "blocked")}},
		"Gate result for a non-Gate cause": {block("git_state"), []any{gateReport(2, "blocked")}},
		"explicit and inline reference": {map[string]any{"cause": "gate2_missing_tests", "headSha": strings.Repeat("c", 40),
			"gateResultId": "01JBQ7Z8X9Y0A1B2C3D4E5F6G7"}, []any{gateReport(2, "blocked")}},
	} {
		t.Run(name, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "verifying")
			input := withGateSHAs(resultBody("verifying", "blocked"))
			input["block"], input["gateResults"] = c.block, c.gates
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 400, ValidationFailed)
		})
	}
	// An explicit reference must point at a result of the cause's Gate.
	f, token, id, _ := resultFixture(t, "verifying")
	gateID := "01JBQ7Z8X9Y0A1B2C3D4E5F6G7"
	if err := store.RecordGateResult(t.Context(), f.db, store.GateResult{ID: gateID, ExecutionID: id, Gate: 3, Result: "pass",
		BaseSHA: strings.Repeat("a", 40), TargetSHA: strings.Repeat("c", 40)}); err != nil {
		t.Fatal(err)
	}
	input := resultBody("verifying", "blocked")
	input["block"] = map[string]any{"cause": "gate2_missing_tests", "headSha": strings.Repeat("c", 40), "gateResultId": gateID}
	status, body := postResult(t, f, token, id, input)
	assertAuthError(t, status, body, 422, PreconditionFailed)
}

func TestResultSavesGate3BlockAndGate2Failure(t *testing.T) {
	for _, result := range []string{"blocked", "failed"} {
		t.Run(result, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "verifying")
			input := withGateSHAs(resultBody("verifying", result))
			gate := 2
			if result == "blocked" {
				gate = 3
				input["block"] = map[string]any{"cause": "gate3_mismatch", "headSha": strings.Repeat("c", 40)}
			}
			input["gateResults"] = []any{gateReport(gate, result)}
			if result == "blocked" {
				input["gateResults"] = []any{gateReport(2, "pass"), gateReport(3, "blocked")}
			}
			if status, body := postResult(t, f, token, id, input); status != 204 {
				t.Fatalf("%d %s", status, body)
			}
			var gateID, savedResult, findings string
			if err := f.db.QueryRow(`SELECT id, result, findings FROM gate_results WHERE execution_id = ? AND gate = ?`, id, gate).
				Scan(&gateID, &savedResult, &findings); err != nil {
				t.Fatal(err)
			}
			if savedResult != result || findings != "[]" {
				t.Fatalf("result=%s findings=%s", savedResult, findings)
			}
			if result == "blocked" {
				var linked string
				if err := f.db.QueryRow(`SELECT gate_result_id FROM execution_blocks WHERE execution_id = ?`, id).Scan(&linked); err != nil || linked != gateID {
					t.Fatalf("linked=%s want=%s err=%v", linked, gateID, err)
				}
			}
		})
	}
}

func TestResultLinksPreviouslySavedGateResult(t *testing.T) {
	f, token, id, _ := resultFixture(t, "verifying")
	gateID := "01JBQ7Z8X9Y0A1B2C3D4E5F6G7"
	if err := store.RecordGateResult(t.Context(), f.db, store.GateResult{ID: gateID, ExecutionID: id, Gate: 2, Result: "blocked",
		BaseSHA: strings.Repeat("a", 40), TargetSHA: strings.Repeat("c", 40)}); err != nil {
		t.Fatal(err)
	}
	input := resultBody("verifying", "blocked")
	input["block"] = map[string]any{"cause": "gate2_missing_tests", "headSha": strings.Repeat("c", 40), "gateResultId": gateID}
	if status, body := postResult(t, f, token, id, input); status != 204 {
		t.Fatalf("%d %s", status, body)
	}
	var linked string
	if err := f.db.QueryRow(`SELECT gate_result_id FROM execution_blocks WHERE execution_id = ?`, id).Scan(&linked); err != nil || linked != gateID {
		t.Fatalf("linked=%s err=%v", linked, err)
	}
}

func TestResultRollsBackGateResultsWithReport(t *testing.T) {
	for i, trigger := range []string{
		`BEFORE INSERT ON gate_results`,
		`BEFORE INSERT ON execution_events WHEN NEW.type = 'gate_result'`,
		`BEFORE INSERT ON execution_blocks`,
		`BEFORE INSERT ON notification_requests`,
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "verifying")
			before := readResultSnapshot(t, f, id)
			if _, err := f.db.Exec(`CREATE TRIGGER reject_gate_report ` + trigger + ` BEGIN SELECT RAISE(ABORT, 'gate storage unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			input := withGateSHAs(resultBody("verifying", "blocked"))
			input["gateResults"] = []any{gateReport(2, "blocked")}
			input["block"] = map[string]any{"cause": "gate2_missing_tests", "headSha": strings.Repeat("c", 40)}
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 500, Internal)
			var gates int
			var head sql.NullString
			if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM gate_results WHERE execution_id = ?), harness_head_sha FROM executions WHERE id = ?`, id, id).
				Scan(&gates, &head); err != nil {
				t.Fatal(err)
			}
			if after := readResultSnapshot(t, f, id); after != before || gates != 0 || head.Valid {
				t.Fatalf("partial report survived: %+v gates=%d head=%v", after, gates, head)
			}
		})
	}
}
