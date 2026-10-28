package api

import (
	"fmt"
	"testing"
)

func TestResultRejectsInvalidUsageWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		usage          any
	}{
		{"missing", "publishing", "succeeded", nil},
		{"missing_duration", "publishing", "succeeded", map[string]any{}},
		{"null_duration", "publishing", "succeeded", map[string]any{"wallTimeMs": nil}},
		{"negative_duration", "publishing", "succeeded", map[string]any{"wallTimeMs": -1}},
		{"fractional_duration", "publishing", "succeeded", map[string]any{"wallTimeMs": 0.5}},
		{"overflow_duration", "publishing", "succeeded", map[string]any{"wallTimeMs": uint64(1 << 63)}},
		{"negative_harness_duration", "publishing", "succeeded", map[string]any{"wallTimeMs": 0, "harnessWallTimeMs": -1}},
		{"negative_adapter_duration", "publishing", "succeeded", map[string]any{"wallTimeMs": 0, "harnessUsage": map[string]any{"wallTimeMs": -1}}},
		{"negative_input_tokens", "publishing", "succeeded", map[string]any{"wallTimeMs": 0, "harnessUsage": map[string]any{"inputTokens": -1}}},
		{"negative_output_tokens", "publishing", "succeeded", map[string]any{"wallTimeMs": 0, "harnessUsage": map[string]any{"outputTokens": -1}}},
		{"progress", "claimed", "preparing", map[string]any{"wallTimeMs": 10}},
		{"failure", "publishing", "failed", map[string]any{"wallTimeMs": 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, tc.from)
			before := readResultSnapshot(t, f, id)
			input := resultBody(tc.from, tc.to)
			input["usage"] = tc.usage
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 400, ValidationFailed)
			if after := readResultSnapshot(t, f, id); after != before {
				t.Fatalf("invalid usage mutated DB: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestResultUsageAndSuccessRollBackTogether(t *testing.T) {
	for i, trigger := range []string{
		`BEFORE INSERT ON usage_records`,
		`BEFORE UPDATE OF status ON tasks`,
		`BEFORE INSERT ON notification_requests`,
		`BEFORE INSERT ON execution_events WHEN json_extract(NEW.payload, '$.reason') = 'runner_result'`,
		`BEFORE INSERT ON audit_logs WHEN NEW.action = 'execution.result'`,
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "publishing")
			before := readResultSnapshot(t, f, id)
			if _, err := f.db.Exec(`CREATE TRIGGER reject_usage_result ` + trigger + ` BEGIN SELECT RAISE(ABORT, 'storage unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			status, body := postResult(t, f, token, id, resultBody("publishing", "succeeded"))
			assertAuthError(t, status, body, 500, Internal)
			if after := readResultSnapshot(t, f, id); after != before {
				t.Fatalf("usage/result partially committed: before=%+v after=%+v", before, after)
			}
		})
	}
}
