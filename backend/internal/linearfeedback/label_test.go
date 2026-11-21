package linearfeedback_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearfeedback"
	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const labelKey = "linear-1:label:decision-1"

// labelLinear exercises the real GraphQL client, with independent failures
// for reads and the additive mutation. It preserves labels as a set.
type labelLinear struct {
	fakeLinear
	available             []linear.Label
	current               []string
	adds                  []map[string]any
	queries               []string
	teamReads             int
	teamStatus, addStatus int
	addBody               string
	cutAdds               int
	applyBeforeCut        bool
	archived, removed     bool
	onTeamLabels, onAdd   func()
}

func newLabelLinear() *labelLinear {
	return &labelLinear{
		fakeLinear: fakeLinear{commentStatus: http.StatusOK, commentBody: okComment},
		available: []linear.Label{{ID: "trigger", Name: "ai-ready"},
			{ID: "wrong-case", Name: "Route:project"}, {ID: "wrong-space", Name: "route:project "},
			{ID: "target", Name: "route:project"}},
		current: []string{"ai-ready", "route:owner", "bug"},
	}
}

func (f *labelLinear) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Query     string
		Variables map[string]any
	}
	if json.Unmarshal(raw, &req) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Query, "EvaloomTeamLabels") && f.onTeamLabels != nil {
		f.onTeamLabels()
	}
	if strings.Contains(req.Query, "issueAddLabel") && f.onAdd != nil {
		f.onAdd()
	}
	if !strings.Contains(req.Query, "EvaloomTeamLabels") && !strings.Contains(req.Query, "issueAddLabel") &&
		!strings.Contains(req.Query, "query EvaloomIssue(") {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		f.fakeLinear.ServeHTTP(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, req.Query)
	switch {
	case strings.Contains(req.Query, "issueAddLabel"):
		f.adds = append(f.adds, req.Variables)
		if f.cutAdds > 0 {
			f.cutAdds--
			if f.applyBeforeCut {
				f.current = append(f.current, "route:project")
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		if f.addStatus != 0 {
			w.WriteHeader(f.addStatus)
		}
		if f.addBody != "" {
			io.WriteString(w, f.addBody)
			return
		}
		if f.addStatus == 0 {
			f.current = append(f.current, "route:project")
		}
		io.WriteString(w, `{"data":{"issueAddLabel":{"success":true}}}`)
	case strings.Contains(req.Query, "EvaloomTeamLabels"):
		f.teamReads++
		if f.teamStatus != 0 {
			w.WriteHeader(f.teamStatus)
			return
		}
		if req.Variables["id"] != "team-a" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"team": map[string]any{
			"id": "team-a", "labels": map[string]any{"nodes": f.available,
				"pageInfo": map[string]any{"hasNextPage": false}}}}})
	default:
		f.issueReads++
		if f.removed {
			io.WriteString(w, `{"data":{"issue":null}}`)
			return
		}
		var data map[string]any
		json.Unmarshal([]byte(issueJSON(f.archived)), &data)
		issue := data["data"].(map[string]any)["issue"].(map[string]any)
		nodes := make([]map[string]string, 0, len(f.current))
		for _, name := range f.current {
			nodes = append(nodes, map[string]string{"name": name})
		}
		issue["labels"] = map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}
		json.NewEncoder(w).Encode(data)
	}
}

func setupLabel(t *testing.T, fake *labelLinear) (*sql.DB, *linearfeedback.Worker, *clock) {
	t.Helper()
	db, worker, c, _ := setup(t, fake)
	mustExec(t, db, `UPDATE linear_outbox SET status='sent' WHERE action='comment'`)
	mustExec(t, db, `INSERT INTO routes VALUES ('route:project','project',NULL,'t','t')`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := linearoutbox.EnqueueRouteLabelTx(context.Background(), tx, "linear-1", "decision-1", "route:project"); err != nil {
		t.Fatal(err)
	}
	// Replay must not create another row for the approved decision.
	if err := linearoutbox.EnqueueRouteLabelTx(context.Background(), tx, "linear-1", "decision-1", "route:project"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return db, worker, c
}

func TestLabelAddsOnlyApprovedLabelAndPreservesExistingLabels(t *testing.T) {
	fake := newLabelLinear()
	db, worker, c := setupLabel(t, fake)
	gate := &linear.AuthAlertGate{}
	gate.Acquire()
	worker.AuthGate = gate
	sendDue(t, worker)
	c.advance(time.Hour)
	sendDue(t, worker)
	if len(fake.adds) != 1 || !reflect.DeepEqual(fake.adds[0], map[string]any{"id": "issue-1", "labelId": "target"}) {
		t.Fatalf("add mutations=%v", fake.adds)
	}
	if !reflect.DeepEqual(fake.current, []string{"ai-ready", "route:owner", "bug", "route:project"}) {
		t.Fatalf("labels replaced or removed: %v", fake.current)
	}
	for _, query := range fake.queries {
		if strings.Contains(query, "issueRemoveLabel") || strings.Contains(query, "issueUpdate") || strings.Contains(query, "labelIds") {
			t.Fatalf("non-additive operation: %s", query)
		}
	}
	if r := outboxRow(t, db, labelKey); r.status != "sent" || r.attempts != 1 || r.outcomeUnknown != 0 || r.commentID.Valid || !r.sentAt.Valid {
		t.Fatalf("row=%+v", r)
	}
	if !gate.Acquire() {
		t.Fatal("label success did not release auth suppression")
	}
	if len(notificationEvents(t, db)) != 0 {
		t.Fatal("route label has no execution event")
	}
}

func TestLabelAlreadyPresentIsSentWithoutMutation(t *testing.T) {
	fake := newLabelLinear()
	fake.current = append(fake.current, "route:project")
	db, worker, _ := setupLabel(t, fake)
	sendDue(t, worker)
	if r := outboxRow(t, db, labelKey); r.status != "sent" || r.outcomeUnknown != 0 || len(fake.adds) != 0 || fake.teamReads != 0 {
		t.Fatalf("row=%+v adds=%v team reads=%d", r, fake.adds, fake.teamReads)
	}
}

func TestLabelNeverSendsTriggerFromPreviouslyAllowedRow(t *testing.T) {
	for _, restoreDefaults := range []bool{false, true} {
		t.Run(map[bool]string{false: "custom trigger", true: "default trigger"}[restoreDefaults], func(t *testing.T) {
			fake := newLabelLinear()
			db, worker, _ := setupLabel(t, fake)
			// A row can survive a settings change or an older implementation.
			// It must be rejected before even reading its already-present label.
			mustExec(t, db, `UPDATE routes SET label='ai-ready' WHERE label='route:project'`)
			mustExec(t, db, `INSERT INTO settings VALUES ('linear.triggerLabel','"go"','t'),
		('linear.routingLabelPrefix','"ai-"','t')`)
			mustExec(t, db, `UPDATE linear_outbox SET label_name='ai-ready' WHERE dedupe_key=?`, labelKey)
			if restoreDefaults {
				mustExec(t, db, `DELETE FROM settings WHERE key IN ('linear.triggerLabel','linear.routingLabelPrefix')`)
			}
			sendDue(t, worker)
			if r := outboxRow(t, db, labelKey); r.status != "failed" || r.outcomeUnknown != 0 || fake.issueReads != 0 || len(fake.adds) != 0 {
				t.Fatalf("trigger row=%+v reads=%d adds=%v", r, fake.issueReads, fake.adds)
			}
			if failure := readFailureRecord(t, db, labelKey); failure.failureReason.String != "other" {
				t.Fatalf("trigger failure=%+v", failure)
			}
		})
	}
}

func TestLabelAuthFailureUsesSharedSuppression(t *testing.T) {
	fake := newLabelLinear()
	fake.addStatus = http.StatusUnauthorized
	db, worker, _ := setupLabel(t, fake)
	alerter := &recordingAuthAlerter{}
	gate := &linear.AuthAlertGate{}
	worker.AuthGate, worker.Alerter = gate, alerter
	queue := func(decision string) {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := linearoutbox.EnqueueRouteLabelTx(context.Background(), tx, "linear-1", decision, "route:project"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	queue("decision-2")
	sendDue(t, worker)
	alerter.want(t, "comment")
	queue("decision-3")
	sendDue(t, worker)
	alerter.want(t, "comment")
	// A successful label delivery releases the same gate used by intake.
	fake.addStatus = 0
	queue("decision-4")
	sendDue(t, worker)
	alerter.want(t, "comment")
	fake.addStatus = http.StatusUnauthorized
	fake.current = fake.current[:3]
	queue("decision-5")
	sendDue(t, worker)
	alerter.want(t, "comment", "comment")
}

func TestLabelRevalidatesRoutesAndSettingsImmediatelyBeforeSend(t *testing.T) {
	for _, when := range []string{"before issue read", "during label lookup"} {
		for _, change := range []string{
			`DELETE FROM routes WHERE label='route:project'`,
			`INSERT INTO settings VALUES ('linear.triggerLabel','"route:project"','t')`,
			`INSERT INTO settings VALUES ('linear.routingLabelPrefix','"other:"','t')`,
		} {
			t.Run(when+"/"+change, func(t *testing.T) {
				fake := newLabelLinear()
				db, worker, c := setupLabel(t, fake)
				if when == "during label lookup" {
					fake.onTeamLabels = func() { mustExec(t, db, change) }
				} else {
					mustExec(t, db, change)
				}
				sendDue(t, worker)
				c.advance(time.Hour)
				sendDue(t, worker)
				r := outboxRow(t, db, labelKey)
				failure := readFailureRecord(t, db, labelKey)
				if r.status != "failed" || r.attempts != 1 || r.outcomeUnknown != 0 || r.nextAttemptAt.Valid ||
					failure.failureReason.String != "other" || !failure.failedAt.Valid || len(fake.adds) != 0 {
					t.Fatalf("row=%+v failure=%+v adds=%v", r, failure, fake.adds)
				}
				if when == "before issue read" && fake.issueReads != 0 {
					t.Fatal("invalid route caused an external request")
				}
			})
		}
	}
}

func TestLabelCutConnectionKeepsOutcomeKnownAndRechecksOnRestart(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "not applied", true: "applied"}[applied], func(t *testing.T) {
			fake := newLabelLinear()
			fake.cutAdds, fake.applyBeforeCut = 1, applied
			db, worker, c := setupLabel(t, fake)
			fake.onAdd = func() {
				if r := outboxRow(t, db, labelKey); r.outcomeUnknown != 0 {
					t.Errorf("outcome_unknown during send=%d", r.outcomeUnknown)
				}
			}
			sendDue(t, worker)
			if r := outboxRow(t, db, labelKey); r.status != "pending" || r.attempts != 1 || r.outcomeUnknown != 0 || !r.nextAttemptAt.Valid {
				t.Fatalf("after lost response: %+v", r)
			}
			c.advance(time.Minute)
			restarted := &linearfeedback.Worker{DB: db, Client: worker.Client, Now: c.now, Logger: worker.Logger}
			sendDue(t, restarted)
			want := 2
			if applied {
				want = 1
			}
			if r := outboxRow(t, db, labelKey); r.status != "sent" || r.outcomeUnknown != 0 || len(fake.adds) != want || fake.commentReads != 0 {
				t.Fatalf("after restart: %+v adds=%d comment reads=%d", r, len(fake.adds), fake.commentReads)
			}
		})
	}
}

func TestLabelFailuresDoNotChangeTaskRouteOrExecution(t *testing.T) {
	for _, scenario := range []struct {
		name, reason string
		attempts     int
		configure    func(*labelLinear)
	}{
		{"missing exact name", "label_not_found", 1, func(f *labelLinear) { f.available = f.available[:3] }},
		{"auth", "auth", 1, func(f *labelLinear) { f.addStatus = http.StatusUnauthorized }},
		{"lookup auth", "auth", 1, func(f *labelLinear) { f.teamStatus = http.StatusForbidden }},
		{"archived", "issue_removed", 1, func(f *labelLinear) { f.archived = true }},
		{"deleted", "issue_removed", 1, func(f *labelLinear) { f.removed = true }},
		{"lookup fails", "retries_exhausted", 5, func(f *labelLinear) { f.teamStatus = http.StatusServiceUnavailable }},
		{"mutation fails", "retries_exhausted", 5, func(f *labelLinear) { f.addStatus = http.StatusServiceUnavailable }},
		{"rate limited", "retries_exhausted", 5, func(f *labelLinear) { f.addStatus = http.StatusTooManyRequests }},
		{"success false", "retries_exhausted", 5, func(f *labelLinear) { f.addBody = `{"data":{"issueAddLabel":{"success":false}}}` }},
		{"external missing label message", "retries_exhausted", 5, func(f *labelLinear) {
			f.addBody = `{"errors":[{"message":"label_not_found: linearfeedback: label name not found"}]}`
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fake := newLabelLinear()
			scenario.configure(fake)
			db, worker, c := setupLabel(t, fake)
			before := taskStates(t, db)
			var destinationBefore, destinationAfter string
			if err := db.QueryRow(`SELECT json_array(project_id,harness_id) FROM tasks WHERE id='linear-1'`).Scan(&destinationBefore); err != nil {
				t.Fatal(err)
			}
			gate := &linear.AuthAlertGate{}
			gate.Acquire()
			worker.AuthGate = gate
			for attempt := range linearfeedback.MaxAttempts {
				sendDue(t, worker)
				if attempt < scenario.attempts-1 {
					want := c.now().Add(linearfeedback.RetryDelays[attempt]).Format("2006-01-02T15:04:05.000000000Z")
					if got := outboxRow(t, db, labelKey); got.status != "pending" || got.nextAttemptAt.String != want || got.outcomeUnknown != 0 {
						t.Fatalf("retry %d: %+v want next=%s", attempt+1, got, want)
					}
				}
				c.advance(time.Hour)
			}
			failure := readFailureRecord(t, db, labelKey)
			r := outboxRow(t, db, labelKey)
			if r.status != "failed" || r.attempts != scenario.attempts || r.outcomeUnknown != 0 || r.nextAttemptAt.Valid ||
				failure.failureReason.String != scenario.reason || !failure.failedAt.Valid {
				t.Fatalf("row=%+v failure=%+v", r, failure)
			}
			failures, err := store.ListNotificationFailures(context.Background(), db, nil, 10)
			if err != nil || len(failures) != 1 || failures[0].Reason != scenario.reason || failures[0].Kind != "route_approved" {
				t.Fatalf("failures=%+v err=%v", failures, err)
			}
			if err := db.QueryRow(`SELECT json_array(project_id,harness_id) FROM tasks WHERE id='linear-1'`).Scan(&destinationAfter); err != nil {
				t.Fatal(err)
			}
			if taskStates(t, db) != before || destinationAfter != destinationBefore {
				t.Fatal("label failure changed task route or execution")
			}
			if len(notificationEvents(t, db)) != 0 {
				t.Fatal("label failure wrote an execution event")
			}
			if gate.Acquire() {
				t.Fatal("label failure released auth suppression")
			}
			if scenario.reason == "label_not_found" && len(fake.adds) != 0 {
				t.Fatal("missing label was sent")
			}
		})
	}
}
