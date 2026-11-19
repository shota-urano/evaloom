package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// Q229: availability follows the configured Runner, independently of other
// online Runners, proposer configuration, or whether any Issue is waiting.
func TestLinearStatusRoutingRunnerAvailability(t *testing.T) {
	f := newAuthFixture(t)
	execSQL(t, f, `INSERT INTO runners (id, name, token_hash, status, created_at) VALUES
		('other', 'Other Runner', 'private-other-token', 'online', 't'),
		('configured', 'Configured Runner', 'private-configured-token', 'offline', 't')`)
	for _, test := range []struct {
		name, runnerStatus string
		runnerID           any
		want               *linearRoutingRunnerProblemView
	}{
		{"unset", "offline", nil, &linearRoutingRunnerProblemView{"unset", "振り分けの提案を動かす Runner が未設定です"}},
		{"missing", "offline", "missing", &linearRoutingRunnerProblemView{"not_found", "振り分けの提案を動かす Runner が見つかりません"}},
		{"offline", "offline", "configured", &linearRoutingRunnerProblemView{"offline", "振り分けの提案を動かす Runner が offline です"}},
		{"online", "online", "configured", nil},
		{"offline again", "offline", "configured", &linearRoutingRunnerProblemView{"offline", "振り分けの提案を動かす Runner が offline です"}},
		{"cleared", "online", nil, &linearRoutingRunnerProblemView{"unset", "振り分けの提案を動かす Runner が未設定です"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := projectRequest(t, f, "PATCH", "/api/settings",
				map[string]any{"routing": map[string]any{"defaultRunnerId": test.runnerID}})
			if status != 200 {
				t.Fatalf("PATCH status=%d body=%s", status, body)
			}
			if _, err := f.db.Exec(`UPDATE runners SET status = ? WHERE id = 'configured'`, test.runnerStatus); err != nil {
				t.Fatal(err)
			}
			before := readSettingsState(t, f.db)
			status, body = getJSON(t, f, "/api/linear/status")
			var view struct {
				Problem json.RawMessage `json:"routingRunnerProblem"`
			}
			if err := json.Unmarshal(body, &view); status != 200 || err != nil {
				t.Fatalf("GET status=%d body=%s err=%v", status, body, err)
			}
			want, err := json.Marshal(test.want)
			if err != nil {
				t.Fatal(err)
			}
			// A healthy Runner must return an explicit null, not omit the key.
			if string(view.Problem) != string(want) {
				t.Fatalf("routingRunnerProblem=%s; want %s", view.Problem, want)
			}
			if strings.Contains(string(body), "private-") {
				t.Fatalf("Runner credential leaked: %s", body)
			}
			if after := readSettingsState(t, f.db); len(after.audits) != len(before.audits) {
				t.Fatalf("status read added audit rows: before=%+v after=%+v", before, after)
			}
		})
	}
}

// A storage error must not be presented as an absent or healthy Runner.
func TestLinearStatusRoutingRunnerReadFailure(t *testing.T) {
	f := newAuthFixture(t)
	status, body := projectRequest(t, f, "PATCH", "/api/settings",
		map[string]any{"routing": map[string]any{"defaultRunnerId": "configured"}})
	if status != 200 {
		t.Fatalf("PATCH status=%d body=%s", status, body)
	}
	execSQL(t, f, `ALTER TABLE runners RENAME TO unavailable_runners`)
	status, body = getJSON(t, f, "/api/linear/status")
	if status != 500 {
		t.Fatalf("GET status=%d body=%s; want storage error", status, body)
	}
}
