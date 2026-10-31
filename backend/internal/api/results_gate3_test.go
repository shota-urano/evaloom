package api

import (
	"strings"
	"testing"
)

func TestResultGate3RecordsChangedHEADWithoutReplacingTestedSHA(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "ObservedHEAD", true: "WrongBlockHEAD"}[mismatch], func(t *testing.T) {
			f, token, id, input := verifyResultFixture(t)
			before := readResultSnapshot(t, f, id)
			current := strings.Repeat("d", 40)
			gate3 := gateReport(3, "blocked")
			gate3["targetSha"] = current
			input["status"] = "blocked"
			input["gateResults"] = append(input["gateResults"].([]any), gate3)
			input["block"] = map[string]any{"cause": "gate3_mismatch", "headSha": current}
			if mismatch {
				input["block"].(map[string]any)["headSha"] = strings.Repeat("e", 40)
			}
			status, body := postResult(t, f, token, id, input)
			if mismatch {
				assertAuthError(t, status, body, 422, PreconditionFailed)
				if after := readResultSnapshot(t, f, id); after != before {
					t.Fatalf("rejection changed state: %+v", after)
				}
				return
			}
			if status != 204 {
				t.Fatalf("%d %s", status, body)
			}
			var head, verified, target, block string
			if err := f.db.QueryRow(`SELECT e.harness_head_sha,e.verified_sha,g.target_sha,b.head_sha
				FROM executions e JOIN gate_results g ON g.execution_id=e.id AND g.gate=3
				JOIN execution_blocks b ON b.gate_result_id=g.id WHERE e.id=?`, id).Scan(&head, &verified, &target, &block); err != nil {
				t.Fatal(err)
			}
			if head != strings.Repeat("c", 40) || verified != head || target != current || block != current {
				t.Fatalf("head=%s verified=%s target=%s block=%s", head, verified, target, block)
			}
		})
	}
}
