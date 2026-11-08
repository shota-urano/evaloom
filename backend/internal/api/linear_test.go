package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func getJSON(t *testing.T, f *authFixture, path string) (int, []byte) {
	t.Helper()
	return f.request(t, "GET", path, true, "", nil)
}

// settingsState is the settings table and the settings.update audit rows.
type settingsState struct {
	rows   map[string]string
	audits []string
}

func readSettingsState(t *testing.T, db *sql.DB) settingsState {
	t.Helper()
	state := settingsState{rows: map[string]string{}}
	rows, err := db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatal(err)
		}
		state.rows[key] = value
	}
	rows.Close()
	rows, err = db.Query(`SELECT actor || ' ' || target_type || ' ' || target_id || ' ' || detail FROM audit_logs
		WHERE action = 'settings.update' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var audit string
		if err := rows.Scan(&audit); err != nil {
			t.Fatal(err)
		}
		state.audits = append(state.audits, audit)
	}
	return state
}

func TestSettingsLinearLabelsDefaultsAndPatch(t *testing.T) {
	f := newAuthFixture(t)
	// No settings rows: the defaults (spec 15 3.4).
	status, body := getJSON(t, f, "/api/settings")
	if status != 200 || string(body) != `{"linear":{"triggerLabel":"ai-ready","routingLabelPrefix":"repo:"}}`+"\n" {
		t.Fatalf("GET status=%d body=%s", status, body)
	}
	// Omitted keys keep their values.
	status, body = projectRequest(t, f, "PATCH", "/api/settings", map[string]any{"linear": map[string]any{"triggerLabel": "go"}})
	if status != 200 || string(body) != `{"linear":{"triggerLabel":"go","routingLabelPrefix":"repo:"}}`+"\n" {
		t.Fatalf("PATCH status=%d body=%s", status, body)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/settings", map[string]any{"linear": map[string]any{"routingLabelPrefix": "to:"}})
	if status != 200 {
		t.Fatalf("PATCH status=%d body=%s", status, body)
	}
	// The Poller reads the same settings.
	if got := f.settings.LinearLabels(context.Background()); got != (linearintake.Labels{TriggerLabel: "go", RoutingLabelPrefix: "to:"}) {
		t.Fatalf("shared settings = %+v", got)
	}
	if status, body = getJSON(t, f, "/api/settings"); string(body) != `{"linear":{"triggerLabel":"go","routingLabelPrefix":"to:"}}`+"\n" {
		t.Fatalf("GET after PATCH status=%d body=%s", status, body)
	}
	// One settings.update row per changed key, with the values before and
	// after (spec 15 5章, Q76).
	want := settingsState{
		rows: map[string]string{"linear.triggerLabel": `"go"`, "linear.routingLabelPrefix": `"to:"`},
		audits: []string{
			`owner settings linear.triggerLabel {"after":"go","before":"ai-ready"}`,
			`owner settings linear.routingLabelPrefix {"after":"to:","before":"repo:"}`,
		},
	}
	if got := readSettingsState(t, f.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %+v; want %+v", got, want)
	}
	// The same values again: 200 and no audit row.
	status, body = projectRequest(t, f, "PATCH", "/api/settings", map[string]any{"linear": map[string]any{"triggerLabel": "go", "routingLabelPrefix": "to:"}})
	if status != 200 || string(body) != `{"linear":{"triggerLabel":"go","routingLabelPrefix":"to:"}}`+"\n" {
		t.Fatalf("same-value PATCH status=%d body=%s", status, body)
	}
	if got := readSettingsState(t, f.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("same-value PATCH changed state: %+v", got)
	}
	// Changing both keys at once writes one row per key.
	status, _ = projectRequest(t, f, "PATCH", "/api/settings", map[string]any{"linear": map[string]any{"triggerLabel": "go2", "routingLabelPrefix": "to:"}})
	if status != 200 {
		t.Fatalf("PATCH status=%d", status)
	}
	want.rows["linear.triggerLabel"] = `"go2"`
	want.audits = append(want.audits, `owner settings linear.triggerLabel {"after":"go2","before":"go"}`)
	if got := readSettingsState(t, f.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %+v; want %+v", got, want)
	}

	for _, test := range []struct {
		name          string
		body          any
		field, reason string
	}{
		{"null label", map[string]any{"linear": map[string]any{"triggerLabel": nil}}, "linear.triggerLabel", "required"},
		{"blank label", map[string]any{"linear": map[string]any{"triggerLabel": " "}}, "linear.triggerLabel", "required"},
		{"empty label", map[string]any{"linear": map[string]any{"triggerLabel": ""}}, "linear.triggerLabel", "required"},
		{"empty prefix", map[string]any{"linear": map[string]any{"routingLabelPrefix": ""}}, "linear.routingLabelPrefix", "required"},
		{"blank prefix", map[string]any{"linear": map[string]any{"routingLabelPrefix": "\t "}}, "linear.routingLabelPrefix", "required"},
		{"null prefix", map[string]any{"linear": map[string]any{"routingLabelPrefix": nil}}, "linear.routingLabelPrefix", "required"},
		{"number", map[string]any{"linear": map[string]any{"routingLabelPrefix": 1}}, "linear.routingLabelPrefix", "invalid"},
		{"unknown linear key", map[string]any{"linear": map[string]any{"pollInterval": "1m"}}, "body", "invalid_json"},
		{"unknown top-level key", map[string]any{"concurrency": map[string]any{}}, "body", "invalid_json"},
		{"unknown key beside a valid one", map[string]any{"linear": map[string]any{"triggerLabel": "new", "x": "y"}}, "body", "invalid_json"},
		{"unknown top-level key beside linear", map[string]any{"linear": map[string]any{"triggerLabel": "new"}, "x": 1}, "body", "invalid_json"},
		{"linear not an object", map[string]any{"linear": "ai-ready"}, "linear", "invalid"},
		{"linear null", map[string]any{"linear": nil}, "linear", "invalid"},
		// A rejected key leaves the valid key of the same request unapplied.
		{"partly invalid", map[string]any{"linear": map[string]any{"triggerLabel": "new", "routingLabelPrefix": ""}}, "linear.routingLabelPrefix", "required"},
		{"partly invalid trigger", map[string]any{"linear": map[string]any{"triggerLabel": nil, "routingLabelPrefix": "new:"}}, "linear.triggerLabel", "required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := projectRequest(t, f, "PATCH", "/api/settings", test.body)
			requireProjectError(t, status, body, 400, ValidationFailed, test.field, test.reason)
			if got := f.settings.LinearLabels(context.Background()); got != (linearintake.Labels{TriggerLabel: "go2", RoutingLabelPrefix: "to:"}) {
				t.Fatalf("settings changed by a rejected PATCH: %+v", got)
			}
			if got := readSettingsState(t, f.db); !reflect.DeepEqual(got, want) {
				t.Fatalf("rejected PATCH changed the settings or audit_logs: %+v", got)
			}
		})
	}
}

// A settings row holding an empty value is served as the default.
func TestSettingsEmptyStoredValueIsServedAsDefault(t *testing.T) {
	f := newAuthFixture(t)
	if _, err := f.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('linear.routingLabelPrefix', '" "', 't')`); err != nil {
		t.Fatal(err)
	}
	status, body := getJSON(t, f, "/api/settings")
	if status != 200 || string(body) != `{"linear":{"triggerLabel":"ai-ready","routingLabelPrefix":"repo:"}}`+"\n" {
		t.Fatalf("GET status=%d body=%s", status, body)
	}
}

func requireLinearRoute(t *testing.T, p model.Project, team, label *string) {
	t.Helper()
	if !equalOptional(p.LinearTeamID, team) || !equalOptional(p.LinearRoutingLabel, label) {
		t.Fatalf("linear = %v %v; want %v %v", deref(p.LinearTeamID), deref(p.LinearRoutingLabel), deref(team), deref(label))
	}
}

func equalOptional(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func ptr(s string) *string { return &s }

// spec 15 5章: PATCH /api/projects/{id} takes linearTeamId and
// linearRoutingLabel; UNIQUE(linear_team_id, linear_routing_label) answers 409.
func TestProjectsLinearRoutingFields(t *testing.T) {
	f := projectFixture(t)
	a := projectInput(t, "project-a")
	a["linearTeamId"], a["linearRoutingLabel"] = "team-a", "repo:one"
	status, body := projectRequest(t, f, "POST", "/api/projects", a)
	requireLinearRoute(t, requireProject(t, status, body, 201), ptr("team-a"), ptr("repo:one"))

	// Omitted on registration: both null.
	status, body = projectRequest(t, f, "POST", "/api/projects", projectInput(t, "project-b"))
	requireLinearRoute(t, requireProject(t, status, body, 201), nil, nil)
	if !strings.Contains(string(body), `"linearTeamId":null,"linearRoutingLabel":null`) {
		t.Fatalf("body = %s", body)
	}

	// The same pair on a second Project is 409, by registration and update.
	c := projectInput(t, "project-c")
	c["linearTeamId"], c["linearRoutingLabel"] = "team-a", "repo:one"
	status, body = projectRequest(t, f, "POST", "/api/projects", c)
	requireProjectError(t, status, body, 409, Conflict, "linearRoutingLabel", "duplicate")
	if n := projectCount(t, f, "projects"); n != 2 {
		t.Fatalf("projects = %d after rejected registration", n)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-b",
		map[string]any{"linearTeamId": "team-a", "linearRoutingLabel": "repo:one"})
	requireProjectError(t, status, body, 409, Conflict, "linearRoutingLabel", "duplicate")
	if p, err := store.GetProject(context.Background(), f.db, "project-b"); err != nil || p.LinearTeamID != nil {
		t.Fatalf("project-b changed by a rejected update: %+v %v", p, err)
	}

	// Same Team with another label, or the same label in another Team, is fine.
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-b",
		map[string]any{"linearTeamId": "team-a", "linearRoutingLabel": "repo:two"})
	requireLinearRoute(t, requireProject(t, status, body, 200), ptr("team-a"), ptr("repo:two"))
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-b", map[string]any{"linearTeamId": "team-b", "linearRoutingLabel": "repo:one"})
	requireLinearRoute(t, requireProject(t, status, body, 200), ptr("team-b"), ptr("repo:one"))
	// Re-saving a Project's own pair is not a conflict.
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"name": "Renamed"})
	requireLinearRoute(t, requireProject(t, status, body, 200), ptr("team-a"), ptr("repo:one"))

	for _, field := range []string{"linearTeamId", "linearRoutingLabel"} {
		status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{field: "  "})
		requireProjectError(t, status, body, 400, ValidationFailed, field, "required")
	}

	// Removing the Team drops its poll row so a later registration starts
	// over from the first poll (spec 15 3.8, Q64); other Teams keep theirs.
	if _, err := f.db.Exec(`INSERT INTO linear_poll_teams (team_id, baseline_done_at, consecutive_failures)
		VALUES ('team-a', '2026-11-08T00:00:00Z', 0), ('team-b', '2026-11-08T00:00:00Z', 0)`); err != nil {
		t.Fatal(err)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"linearTeamId": nil})
	requireLinearRoute(t, requireProject(t, status, body, 200), nil, ptr("repo:one"))
	var teams string
	if err := f.db.QueryRow(`SELECT group_concat(team_id) FROM linear_poll_teams`).Scan(&teams); err != nil || teams != "team-b" {
		t.Fatalf("poll teams = %q, %v; want team-b", teams, err)
	}
	var detail string
	if err := f.db.QueryRow(`SELECT detail FROM audit_logs WHERE action = 'project.update' AND target_id = 'project-a'
		ORDER BY rowid DESC LIMIT 1`).Scan(&detail); err != nil || !strings.Contains(detail, `"linearTeamId":null`) {
		t.Fatalf("audit detail = %s, %v", detail, err)
	}
}

func TestLinearStatusListsTeamsAndAttachedIssues(t *testing.T) {
	f := newAuthFixture(t)
	status, body := getJSON(t, f, "/api/linear/status")
	if status != 200 || string(body) != `{"teams":[],"issues":[]}`+"\n" {
		t.Fatalf("empty status=%d body=%s", status, body)
	}
	if _, err := f.db.Exec(`INSERT INTO linear_poll_teams (team_id, baseline_done_at, last_success_at, last_error, consecutive_failures)
		VALUES ('team-a', '2026-11-08T00:00:00Z', '2026-11-08T09:00:00.5Z', NULL, 0),
		       ('team-b', NULL, NULL, 'rate_limited', 3)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO linear_issue_states (linear_issue_id, issue_identifier, team_id, ai_ready,
		attach_seq, issue_updated_at, last_decision, last_decision_at, first_seen_at, last_seen_at) VALUES
		('i-2', 'ENG-2', 'team-a', 1, 1, 't', 'held', '2026-11-08T09:01:00Z', 't', 't'),
		('i-1', 'ENG-1', 'team-a', 1, 0, 't', NULL, NULL, 't', 't'),
		('i-3', 'ENG-3', 'team-a', 0, 1, 't', 'cancelled', '2026-11-08T09:02:00Z', 't', 't')`); err != nil {
		t.Fatal(err)
	}
	status, body = getJSON(t, f, "/api/linear/status")
	want := `{"teams":[` +
		`{"teamId":"team-a","lastSuccessAt":"2026-11-08T09:00:00.5Z","lastError":null,"consecutiveFailures":0},` +
		`{"teamId":"team-b","lastSuccessAt":null,"lastError":"rate_limited","consecutiveFailures":3}],` +
		`"issues":[` +
		`{"linearIssueId":"i-1","issueIdentifier":"ENG-1","teamId":"team-a","lastDecision":null,"lastDecisionAt":null},` +
		`{"linearIssueId":"i-2","issueIdentifier":"ENG-2","teamId":"team-a","lastDecision":"held","lastDecisionAt":"2026-11-08T09:01:00Z"}]}` + "\n"
	if status != 200 || string(body) != want {
		t.Fatalf("status=%d body=%s\nwant %s", status, body, want)
	}
	// Owner-only like the other management APIs.
	if status, body := f.request(t, "GET", "/api/linear/status", false, "", nil); status != 401 {
		t.Fatalf("unauthenticated status=%d body=%s", status, body)
	}
}

// A real Linear client polls a test server that rejects the key and echoes
// it back; neither the key nor the response body may reach the status or
// settings responses (spec 15 3.8, 6章).
func TestLinearStatusNeverContainsAPIKey(t *testing.T) {
	const secret = "lin_api_SECRETVALUE123"
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	input["linearTeamId"], input["linearRoutingLabel"] = "team-a", "repo:one"
	if status, body := projectRequest(t, f, "POST", "/api/projects", input); status != 201 {
		t.Fatalf("register status=%d body=%s", status, body)
	}
	linearServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"errors":[{"message":"bad key ` + r.Header.Get("Authorization") + `"}]}`))
	}))
	t.Cleanup(linearServer.Close)
	keyPath := filepath.Join(t.TempDir(), "linear-api-key")
	if err := os.WriteFile(keyPath, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	client, err := linear.NewClient(linearServer.Client(), linearServer.URL, keyPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	poller := &linearintake.Poller{DB: f.db, Client: client, Logger: logger, Settings: f.settings}
	if _, err := poller.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, body := getJSON(t, f, "/api/linear/status")
	var view linearStatusView
	if err := json.Unmarshal(body, &view); err != nil || status != 200 {
		t.Fatalf("status=%d body=%s err=%v", status, body, err)
	}
	if len(view.Teams) != 1 || view.Teams[0].LastError == nil || *view.Teams[0].LastError != "auth" || view.Teams[0].ConsecutiveFailures != 1 {
		t.Fatalf("teams = %s", body)
	}
	_, settingsBody := getJSON(t, f, "/api/settings")
	for name, text := range map[string]string{"status": string(body), "settings": string(settingsBody), "logs": logs.String()} {
		if strings.Contains(text, secret) || strings.Contains(text, "bad key") {
			t.Fatalf("%s contains the API key or Linear response: %s", name, text)
		}
	}
}

// spec 15 5章: the task APIs carry source, intakeBlockCause and linear.
func TestTaskAPIsShowLinearOrigin(t *testing.T) {
	f := taskFixture(t)
	status, body := taskRequest(t, f, "manual", manualTaskInput(), nil)
	requireTask(t, status, body, 201)
	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	held := model.Task{Title: "from linear", Body: "no criteria", ShortName: "eng-7", TaskType: "development",
		CollaborationMode: "solo", Status: model.TaskBlocked, IdempotencyKey: "linear:i-7:1",
		CreatedAt: "2026-11-08T09:00:00Z", UpdatedAt: "2026-11-08T09:00:00Z"}
	if err := store.InsertLinearTask(ctx, tx, &held, "missing_acceptance_criteria"); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertLinearIssueLink(ctx, tx, store.LinearIssueLink{ID: "link-1", LinearIssueID: "i-7",
		IssueIdentifier: "ENG-7", IssueURL: "https://linear.app/x/issue/ENG-7", TeamID: "team-a", TaskID: held.ID,
		AttachSeq: 1, CreatedAt: "2026-11-08T09:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	type origin struct {
		ID               string
		Source           string
		IntakeBlockCause *string
		Linear           *taskLinearView
	}
	check := func(got origin) {
		t.Helper()
		switch got.ID {
		case held.ID:
			if got.Source != "linear" || deref(got.IntakeBlockCause) != "missing_acceptance_criteria" || got.Linear == nil ||
				*got.Linear != (taskLinearView{Identifier: "ENG-7", URL: "https://linear.app/x/issue/ENG-7"}) {
				t.Fatalf("linear task = %+v %v", got, got.Linear)
			}
		default:
			if got.Source != "manual" || got.IntakeBlockCause != nil || got.Linear != nil {
				t.Fatalf("manual task = %+v", got)
			}
		}
	}
	status, body = listTaskRequest(t, f, "")
	var page struct{ Items []origin }
	if err := json.Unmarshal(body, &page); err != nil || status != 200 || len(page.Items) != 2 {
		t.Fatalf("list status=%d body=%s err=%v", status, body, err)
	}
	for _, item := range page.Items {
		check(item)
	}
	if !strings.Contains(string(body), `"intakeBlockCause":null,"linear":null`) {
		t.Fatalf("manual task must carry explicit nulls: %s", body)
	}
	for _, id := range []string{held.ID, page.Items[1].ID} {
		r := httptest.NewRequest("GET", f.server.URL+"/api/tasks/"+id, nil)
		r.AddCookie(f.cookie)
		r.Header.Set("Origin", f.server.URL)
		w := httptest.NewRecorder()
		NewHandler(f.db, nil, f.auth).ServeHTTP(w, r)
		var detail origin
		if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 {
			t.Fatalf("detail status=%d body=%s err=%v", w.Code, w.Body, err)
		}
		check(detail)
	}
}
