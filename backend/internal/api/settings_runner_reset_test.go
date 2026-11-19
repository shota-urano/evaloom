package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linearintake"
)

// Q221: null removes the settings row and audits its before/after values;
// omission preserves it, and clearing an unset key writes nothing.
func TestSettingsDefaultRunnerNullClearsWithAudit(t *testing.T) {
	f := newAuthFixture(t)
	clear := map[string]any{"routing": map[string]any{"defaultRunnerId": nil}}
	assertCleared := func() {
		t.Helper()
		for _, method := range []string{"PATCH", "GET"} {
			var status int
			var body []byte
			if method == "PATCH" {
				status, body = projectRequest(t, f, method, "/api/settings", clear)
			} else {
				status, body = getJSON(t, f, "/api/settings")
			}
			if status != 200 || string(body) != settingsJSON("ai-ready", "route:") {
				t.Fatalf("%s status=%d body=%s", method, status, body)
			}
		}
	}
	assertCleared() // Already unset, including a fresh database.
	if got := readSettingsState(t, f.db); len(got.rows) != 0 || len(got.audits) != 0 {
		t.Fatalf("clearing an unset key wrote %+v", got)
	}
	status, body := projectRequest(t, f, "PATCH", "/api/settings",
		map[string]any{"routing": map[string]any{"defaultRunnerId": "runner-1"}})
	if status != 200 {
		t.Fatalf("set runner status=%d body=%s", status, body)
	}
	before := readSettingsState(t, f.db)
	for _, omitted := range []any{
		map[string]any{},
		map[string]any{"routing": map[string]any{}},
		map[string]any{"linear": map[string]any{"triggerLabel": "ai-ready"}},
	} {
		status, body = projectRequest(t, f, "PATCH", "/api/settings", omitted)
		if got := readSettingsState(t, f.db); status != 200 || !reflect.DeepEqual(got, before) {
			t.Fatalf("omitted runner status=%d body=%s state=%+v", status, body, got)
		}
	}
	assertCleared()
	want := settingsState{rows: map[string]string{}, audits: append(before.audits,
		`owner settings routing.defaultRunnerId {"after":null,"before":"runner-1"}`)}
	if got := readSettingsState(t, f.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("clear state=%+v; want %+v", got, want)
	}
	assertCleared() // Repeating null must not add a second audit row.
	if got := readSettingsState(t, f.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("repeated null changed state=%+v", got)
	}
}

// Invalid Linear values must still reject the whole request, including a
// valid runner reset. Empty/blank Runner IDs must not clear the setting.
func TestSettingsDefaultRunnerResetRejectsInvalidPatchAtomically(t *testing.T) {
	f := newAuthFixture(t)
	status, body := projectRequest(t, f, "PATCH", "/api/settings",
		map[string]any{"routing": map[string]any{"defaultRunnerId": "runner-1"}})
	if status != 200 {
		t.Fatalf("set runner status=%d body=%s", status, body)
	}
	before := readSettingsState(t, f.db)
	for _, test := range []struct {
		name, field string
		patch       any
	}{
		{"null trigger", "linear.triggerLabel", map[string]any{
			"routing": map[string]any{"defaultRunnerId": nil}, "linear": map[string]any{"triggerLabel": nil}}},
		{"null prefix", "linear.routingLabelPrefix", map[string]any{
			"routing": map[string]any{"defaultRunnerId": nil}, "linear": map[string]any{"routingLabelPrefix": nil}}},
		{"empty runner", "routing.defaultRunnerId", map[string]any{
			"routing": map[string]any{"defaultRunnerId": ""}, "linear": map[string]any{"triggerLabel": "go"}}},
		{"blank runner", "routing.defaultRunnerId", map[string]any{
			"routing": map[string]any{"defaultRunnerId": "\t\r\n　"}, "linear": map[string]any{"triggerLabel": "go"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := projectRequest(t, f, "PATCH", "/api/settings", test.patch)
			requireProjectError(t, status, body, 400, ValidationFailed, test.field, "required")
			if got := readSettingsState(t, f.db); !reflect.DeepEqual(got, before) {
				t.Fatalf("rejected PATCH changed state=%+v", got)
			}
		})
	}
}

// Removing a row and its audit must commit together. A failure in either
// write also rolls back other keys changed by the same PATCH.
func TestSettingsDefaultRunnerResetRollsBackOnWriteFailure(t *testing.T) {
	for _, test := range []struct{ name, trigger string }{
		{"delete", `CREATE TRIGGER fail_reset BEFORE DELETE ON settings
			WHEN OLD.key='routing.defaultRunnerId' BEGIN SELECT RAISE(ABORT, 'delete failed'); END`},
		{"audit", `CREATE TRIGGER fail_reset BEFORE INSERT ON audit_logs
			WHEN NEW.target_id='routing.defaultRunnerId' BEGIN SELECT RAISE(ABORT, 'audit failed'); END`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAuthFixture(t)
			status, body := projectRequest(t, f, "PATCH", "/api/settings",
				map[string]any{"routing": map[string]any{"defaultRunnerId": "runner-1"}})
			if status != 200 {
				t.Fatalf("set runner status=%d body=%s", status, body)
			}
			before := readSettingsState(t, f.db)
			execSQL(t, f, test.trigger)
			status, body = projectRequest(t, f, "PATCH", "/api/settings", map[string]any{
				"routing": map[string]any{"defaultRunnerId": nil}, "linear": map[string]any{"triggerLabel": "go"}})
			if status != 500 {
				t.Fatalf("failed write status=%d body=%s", status, body)
			}
			if got := readSettingsState(t, f.db); !reflect.DeepEqual(got, before) {
				t.Fatalf("failed write changed state=%+v", got)
			}
		})
	}
}

// A real poll after resetting the Runner creates a queued proposal with a
// non-null model and a candidate. Claim cannot dispatch it until the Runner
// is configured again (spec 15 6章, Q221).
func TestSettingsDefaultRunnerResetLeavesNewProposalQueued(t *testing.T) {
	f, token, _ := claimFixture(t)
	execSQL(t, f, `INSERT INTO routes VALUES ('route:a','project-a',NULL,'t','t')`)
	status, body := projectRequest(t, f, "PATCH", "/api/settings", map[string]any{"routing": map[string]any{
		"defaultRunnerId": "claim-runner", "proposer": map[string]any{"microHarness": "claude_code", "model": "model-x"}}})
	if status != 200 {
		t.Fatalf("set routing status=%d body=%s", status, body)
	}
	starter := &linearintake.TaskStarter{}
	intake := &linearintake.Intake{DB: f.db, Fetcher: recheckFetcher{}, Starter: starter,
		Canceller: linearintake.TaskCanceller{}, Rechecker: starter}
	rf := &recheckFixture{authFixture: f, client: &recheckClient{}}
	rf.poller = &linearintake.Poller{DB: f.db, Client: rf.client, Handler: intake, Teams: fixedPollTeam("team-a")}
	rf.poll(t) // Finish the first poll before attaching a new Issue.
	status, body = projectRequest(t, f, "PATCH", "/api/settings",
		map[string]any{"routing": map[string]any{"defaultRunnerId": nil}})
	if status != 200 {
		t.Fatalf("reset runner status=%d body=%s", status, body)
	}
	issue := recheckIssue("reset-runner", "team-a")
	rf.poll(t, issue)
	for range 2 {
		status, body = requestClaim(t, f, token)
		requireNoJob(t, status, body)
		rf.poll(t, issue)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM route_proposals WHERE status='queued'
		AND runner_id IS NULL AND finished_at IS NULL AND error_class IS NULL
		AND json_array_length(candidates)=1`); n != 1 {
		t.Fatalf("queued unassigned proposals=%d; want 1", n)
	}
	if got := rf.task(t, issue.ID); got.status != "routing" || got.executions != 0 || got.comments != 0 {
		t.Fatalf("task after reset=%+v", got)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions`); n != 0 {
		t.Fatalf("queued proposal opened %d decisions", n)
	}
	// The existing Dashboard status API reports routing, but its contract
	// has no proposal Runner availability field. Do not add one under Q221.
	status, body = getJSON(t, f, "/api/linear/status")
	var view linearStatusView
	if err := json.Unmarshal(body, &view); status != 200 || err != nil || len(view.Issues) != 1 ||
		view.Issues[0].LastDecision == nil || *view.Issues[0].LastDecision != "routing" {
		t.Fatalf("Dashboard linear status=%d body=%s err=%v", status, body, err)
	}
	// Positive control: setting the Runner again makes this same proposal
	// claimable, proving neither its candidates nor its model blocked it.
	status, body = projectRequest(t, f, "PATCH", "/api/settings",
		map[string]any{"routing": map[string]any{"defaultRunnerId": "claim-runner"}})
	if status != 200 {
		t.Fatalf("restore runner status=%d body=%s", status, body)
	}
	status, body = requestClaim(t, f, token)
	if kind := claimJobKind(t, status, body); kind != "route_proposal" {
		t.Fatalf("restored Runner did not get proposal: status=%d body=%s", status, body)
	}
}
