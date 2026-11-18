package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// routing.proposer and routing.defaultRunnerId are served and replaced by
// GET/PATCH /api/settings, stored in settings and audited per changed key
// (spec 15 5章, 02 3.8, Q76).
func TestSettingsRoutingPatchStoresAndAudits(t *testing.T) {
	f := newAuthFixture(t)
	status, body := projectRequest(t, f, "PATCH", "/api/settings", map[string]any{"routing": map[string]any{
		"proposer": map[string]any{"microHarness": "codex", "model": "model-x"}, "defaultRunnerId": "runner-1"}})
	want := `{"linear":{"triggerLabel":"ai-ready","routingLabelPrefix":"route:"},` +
		`"routing":{"proposer":{"microHarness":"codex","model":"model-x"},"defaultRunnerId":"runner-1"}}` + "\n"
	if status != 200 || string(body) != want {
		t.Fatalf("PATCH status=%d body=%s", status, body)
	}
	if _, body = getJSON(t, f, "/api/settings"); string(body) != want {
		t.Fatalf("GET body=%s", body)
	}
	state := settingsState{
		rows: map[string]string{
			"routing.proposer":        `{"microHarness":"codex","model":"model-x"}`,
			"routing.defaultRunnerId": `"runner-1"`,
		},
		audits: []string{
			`owner settings routing.proposer {"after":{"microHarness":"codex","model":"model-x"},"before":{"microHarness":"claude_code","model":null}}`,
			`owner settings routing.defaultRunnerId {"after":"runner-1","before":null}`,
		},
	}
	if got := readSettingsState(t, f.db); !reflect.DeepEqual(got, state) {
		t.Fatalf("state = %+v; want %+v", got, state)
	}
	// The stored value is what the claim reads for route proposals.
	rows, err := store.ReadSettings(context.Background(), f.db)
	if err != nil || store.RouteProposerModelIsNull(rows) {
		t.Fatalf("proposer model is null after PATCH: %v %v", rows, err)
	}

	// The same values: no audit row.
	status, _ = projectRequest(t, f, "PATCH", "/api/settings", map[string]any{"routing": map[string]any{
		"proposer": map[string]any{"microHarness": "codex", "model": "model-x"}, "defaultRunnerId": "runner-1"}})
	if got := readSettingsState(t, f.db); status != 200 || !reflect.DeepEqual(got, state) {
		t.Fatalf("same-value PATCH status=%d state=%+v", status, got)
	}

	// Back to no model: proposals are no longer made.
	status, _ = projectRequest(t, f, "PATCH", "/api/settings", map[string]any{"routing": map[string]any{
		"proposer": map[string]any{"microHarness": "codex", "model": nil}}})
	if status != 200 {
		t.Fatalf("PATCH status=%d", status)
	}
	state.rows["routing.proposer"] = `{"microHarness":"codex","model":null}`
	state.audits = append(state.audits,
		`owner settings routing.proposer {"after":{"microHarness":"codex","model":null},"before":{"microHarness":"codex","model":"model-x"}}`)
	if got := readSettingsState(t, f.db); !reflect.DeepEqual(got, state) {
		t.Fatalf("state = %+v; want %+v", got, state)
	}
}

// Q160: a prefix change that leaves a routes label outside the prefix is
// 422, and nothing of the request is written.
func TestSettingsPrefixChangeRejectedByRoutes(t *testing.T) {
	f := newAuthFixture(t)
	execSQL(t, f, `INSERT INTO harnesses (id,name,repo_url,repo_path,created_at,updated_at) VALUES ('h','h','url','path','t','t')`)
	execSQL(t, f, `INSERT INTO routes VALUES ('route:x',NULL,'h','t','t')`)
	status, body := projectRequest(t, f, "PATCH", "/api/settings", map[string]any{
		"linear":  map[string]any{"routingLabelPrefix": "to:", "triggerLabel": "go"},
		"routing": map[string]any{"defaultRunnerId": "runner-1"}})
	requireProjectError(t, status, body, 422, PreconditionFailed, "routes.route:x", "outside_routing_label_prefix")
	if got := readSettingsState(t, f.db); len(got.rows) != 0 || len(got.audits) != 0 {
		t.Fatalf("rejected PATCH wrote %+v", got)
	}
	// A prefix every routes label still starts with is accepted.
	status, body = projectRequest(t, f, "PATCH", "/api/settings", map[string]any{"linear": map[string]any{"routingLabelPrefix": "route"}})
	if status != 200 || !strings.Contains(string(body), `"routingLabelPrefix":"route"`) {
		t.Fatalf("PATCH status=%d body=%s", status, body)
	}
}

// A settings row without a value the API can use is served as the default.
func TestSettingsUnusableRoutingRowsAreServedAsDefaults(t *testing.T) {
	f := newAuthFixture(t)
	execSQL(t, f, `INSERT INTO settings VALUES ('routing.proposer','{"model":"x"}','t'),('routing.defaultRunnerId','" "','t')`)
	_, body := getJSON(t, f, "/api/settings")
	if string(body) != settingsJSON("ai-ready", "route:") {
		t.Fatalf("GET body=%s", body)
	}
}

type fixedTeamProblem linearintake.TeamProblem

func (p fixedTeamProblem) PollTeamProblem() linearintake.TeamProblem {
	return linearintake.TeamProblem(p)
}

// GET /api/linear/status shows why intake is stopped when no single Team
// has the key or name uslab (spec 15 3.2, 5章, 6章, Q216).
func TestLinearStatusShowsPollTeamProblem(t *testing.T) {
	f := newAuthFixture(t)
	for _, test := range []struct {
		problem linearintake.TeamProblem
		want    string
	}{
		{"", `"teamProblem":null`},
		{linearintake.TeamNotFound, `"teamProblem":{"reason":"not_found","message":"照会先 Team（uslab）が見つからない"}`},
		{linearintake.TeamMultiple, `"teamProblem":{"reason":"multiple","message":"照会先 Team（uslab）が複数ある"}`},
	} {
		recorder := httptest.NewRecorder()
		getLinearStatus(f.db, nil, fixedTeamProblem(test.problem))(recorder, httptest.NewRequest("GET", "/api/linear/status", nil))
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), test.want) {
			t.Fatalf("problem %q: status=%d body=%s", test.problem, recorder.Code, recorder.Body)
		}
	}
	// Through the Poller: the uslab Team is missing, so nothing is queried
	// and the shared status carries the problem.
	status := &linearintake.PollTeamStatus{}
	poller := &linearintake.Poller{DB: f.db, Client: noUslabClient{}, TeamStatus: status}
	if result, err := poller.PollOnce(context.Background()); err != nil || result.Polled {
		t.Fatalf("PollOnce = %+v, %v", result, err)
	}
	recorder := httptest.NewRecorder()
	getLinearStatus(f.db, nil, status)(recorder, httptest.NewRequest("GET", "/api/linear/status", nil))
	var view struct {
		TeamProblem *struct{ Reason, Message string } `json:"teamProblem"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil || view.TeamProblem == nil ||
		view.TeamProblem.Message != "照会先 Team（uslab）が見つからない" {
		t.Fatalf("body=%s err=%v", recorder.Body, err)
	}
}

type noUslabClient struct{}

func (noUslabClient) CheckKey() error { return nil }
func (noUslabClient) ListReadyIssues(context.Context, string, string) ([]linear.Issue, error) {
	panic("queried without a polling Team")
}
func (noUslabClient) ListTeams(context.Context) ([]linear.Team, error) {
	return []linear.Team{{ID: "eng", Key: "ENG", Name: "Engineering"}}, nil
}
