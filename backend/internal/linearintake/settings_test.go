package linearintake

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
)

// A key without a settings row uses its default (spec 02 3.4, spec 15 3.4).
func TestLabelSettingsWithoutRowsUseDefaults(t *testing.T) {
	db := openDB(t)
	if got := NewLabelSettings(db, nil).LinearLabels(context.Background()); got != (Labels{TriggerLabel: "ai-ready", RoutingLabelPrefix: "repo:"}) {
		t.Fatalf("defaults = %+v", got)
	}
	if got := roundLabels(context.Background()); got != DefaultLabels() {
		t.Fatalf("round labels without a round = %+v", got)
	}
	// Only one key stored: the other keeps its default.
	exec(t, db, `INSERT INTO settings (key, value, updated_at) VALUES ('linear.routingLabelPrefix', '"to:"', 't')`)
	if got := NewLabelSettings(db, nil).LinearLabels(context.Background()); got != (Labels{TriggerLabel: "ai-ready", RoutingLabelPrefix: "to:"}) {
		t.Fatalf("one stored key = %+v", got)
	}
}

// Rows that are empty, blank or not a JSON string, and an unreadable table,
// fall back to the defaults key by key and are logged (spec 15 6章, Q76).
// Unknown keys are ignored (spec 02 3.4).
func TestLabelSettingsUnusableRowsUseDefaultsAndLog(t *testing.T) {
	for _, test := range []struct{ name, trigger, prefix string }{
		{"empty string", `""`, `"to:"`},
		{"blank string", `"  "`, `"to:"`},
		{"empty text", ``, `"to:"`},
		{"number", `1`, `"to:"`},
		{"broken JSON", `"go`, `"to:"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openDB(t)
			exec(t, db, `INSERT INTO settings (key, value, updated_at) VALUES
				('linear.triggerLabel', ?, 't'), ('linear.routingLabelPrefix', ?, 't'), ('linear.unknown', '""', 't')`,
				test.trigger, test.prefix)
			var logs bytes.Buffer
			settings := NewLabelSettings(db, slog.New(slog.NewTextHandler(&logs, nil)))
			if got := settings.LinearLabels(context.Background()); got != (Labels{TriggerLabel: "ai-ready", RoutingLabelPrefix: "to:"}) {
				t.Fatalf("labels = %+v", got)
			}
			if !strings.Contains(logs.String(), "key=linear.triggerLabel") || strings.Contains(logs.String(), "linear.routingLabelPrefix") ||
				strings.Contains(logs.String(), "linear.unknown") {
				t.Fatalf("logs = %s", logs.String())
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		db := openDB(t)
		exec(t, db, `DROP TABLE settings`)
		var logs bytes.Buffer
		settings := NewLabelSettings(db, slog.New(slog.NewTextHandler(&logs, nil)))
		if got := settings.LinearLabels(context.Background()); got != DefaultLabels() {
			t.Fatalf("labels = %+v", got)
		}
		if !strings.Contains(logs.String(), "settings could not be read") {
			t.Fatalf("logs = %s", logs.String())
		}
	})
}

type auditRow struct{ actor, action, targetType, targetID, before, after string }

func settingsAudits(t *testing.T, db *sql.DB) []auditRow {
	t.Helper()
	rows, err := db.Query(`SELECT actor, action, target_type, target_id, json_extract(detail, '$.before'),
		json_extract(detail, '$.after') FROM audit_logs WHERE action = 'settings.update' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var audits []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.actor, &a.action, &a.targetType, &a.targetID, &a.before, &a.after); err != nil {
			t.Fatal(err)
		}
		audits = append(audits, a)
	}
	return audits
}

// Each changed key gets its row and one settings.update audit row with the
// values before and after; an unchanged value writes nothing (spec 15 5章,
// spec 02 3.4, Q76).
func TestUpdateLinearLabelsWritesChangedKeysWithAudit(t *testing.T) {
	db := openDB(t)
	settings := NewLabelSettings(db, nil)
	ctx := context.Background()
	update := func(patch LabelsPatch) Labels {
		t.Helper()
		labels, err := settings.UpdateLinearLabels(ctx, patch)
		if err != nil {
			t.Fatal(err)
		}
		return labels
	}
	goLabel, toPrefix, same := "go", "to:", "ai-ready"
	// The default value itself is not a change.
	if got := update(LabelsPatch{TriggerLabel: &same}); got != DefaultLabels() {
		t.Fatalf("labels = %+v", got)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM settings`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("settings rows = %d err=%v", rows, err)
	}
	if got := update(LabelsPatch{TriggerLabel: &goLabel, RoutingLabelPrefix: &toPrefix}); got != (Labels{"go", "to:"}) {
		t.Fatalf("labels = %+v", got)
	}
	update(LabelsPatch{TriggerLabel: &goLabel}) // same value again
	want := []auditRow{
		{"owner", "settings.update", "settings", "linear.triggerLabel", "ai-ready", "go"},
		{"owner", "settings.update", "settings", "linear.routingLabelPrefix", "repo:", "to:"},
	}
	if got := settingsAudits(t, db); len(got) != 2 || !slices.Contains(got, want[0]) || !slices.Contains(got, want[1]) {
		t.Fatalf("audits = %+v", got)
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'linear.triggerLabel'`).Scan(&value); err != nil || value != `"go"` {
		t.Fatalf("stored value = %q err=%v", value, err)
	}
	// A failing audit write rolls the settings row back (one transaction).
	exec(t, db, `CREATE TRIGGER fail_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT, 'audit'); END`)
	other := "other"
	if _, err := settings.UpdateLinearLabels(ctx, LabelsPatch{TriggerLabel: &other}); err == nil {
		t.Fatal("update succeeded without its audit row")
	}
	if got := settings.LinearLabels(ctx); got != (Labels{"go", "to:"}) {
		t.Fatalf("labels after a failed update = %+v", got)
	}
}

// settings holding an empty value (written directly to the DB): the Poller
// queries with the default for that key and the Control Plane logs it
// (spec 15 6章, Q76). Polling does not stop.
func TestPollerUsesDefaultForEmptyStoredSetting(t *testing.T) {
	db := startDB(t)
	exec(t, db, `INSERT INTO settings (key, value, updated_at) VALUES ('linear.triggerLabel', '""', 't')`)
	var logs bytes.Buffer
	client := &stubClient{}
	p := newPoller(db, client, &logs)
	p.Settings = NewLabelSettings(db, p.Logger)
	pollOnce(t, p)
	calls, _, _ := client.snapshot()
	if len(calls) != 2 || calls[0] != "team-a/ai-ready" || calls[1] != "team-b/ai-ready" {
		t.Fatalf("calls = %v", calls)
	}
	if !strings.Contains(logs.String(), "setting is empty; using the default") || !strings.Contains(logs.String(), "key=linear.triggerLabel") {
		t.Fatalf("logs = %s", logs.String())
	}
}

// The Poller reads the global settings linear.triggerLabel and
// linear.routingLabelPrefix (spec 15 3.4, 5章, Q48) and the whole round, up
// to TaskStarter's routing and Intake's detach check, uses them.
func TestPollerUsesGlobalLabelSettings(t *testing.T) {
	db := startDB(t)
	exec(t, db, `UPDATE projects SET linear_routing_label = 'to:one' WHERE id = 'p1'`)
	settings := NewLabelSettings(db, nil)
	goLabel, toPrefix := "go", "to:"
	if _, err := settings.UpdateLinearLabels(context.Background(), LabelsPatch{TriggerLabel: &goLabel, RoutingLabelPrefix: &toPrefix}); err != nil {
		t.Fatal(err)
	}

	issue := linear.Issue{ID: "1", Identifier: "ENG-1", URL: "https://linear.app/x/ENG-1", Title: "t",
		Description: criteriaBody, TeamID: "team-a", Labels: []string{"go", "to:one", "repo:three"},
		StateType: "unstarted", UpdatedAt: base}
	var listed []linear.Issue
	client := &stubClient{list: func(teamID string) ([]linear.Issue, error) {
		if teamID == "team-a" {
			return listed, nil
		}
		return nil, nil
	}}
	fetcher := &stubFetcher{issues: map[string]linear.Issue{}, errs: map[string]error{}}
	starter := &TaskStarter{Now: func() time.Time { return base }}
	p := newPoller(db, client, nil)
	p.Settings = settings
	p.Handler = &Intake{DB: db, Fetcher: fetcher, Starter: starter, Canceller: TaskCanceller{}, Rechecker: starter,
		Now: func() time.Time { return base }}

	pollOnce(t, p) // first poll of the Team: baseline only (Q64)
	listed = []linear.Issue{issue}
	pollOnce(t, p)
	calls, _, _ := client.snapshot()
	for _, call := range calls {
		if !strings.HasSuffix(call, "/go") {
			t.Fatalf("calls = %v; want the trigger label from the settings", calls)
		}
	}
	// "repo:three" is not a routing label under the prefix "to:", so the
	// Issue routes to p1 alone instead of being held for two labels.
	got := readStarted(t, db, "1", 1)
	if got.status != "queued" || got.project.String != "p1" || got.cause.Valid {
		t.Fatalf("task = %+v", got)
	}

	// The Issue leaves the list still carrying ai-ready but not "go": with
	// the settings it is a removal of the trigger label.
	listed = nil
	refetched := issue
	refetched.Labels = []string{"ai-ready", "to:one"}
	fetcher.issues["1"] = refetched
	pollOnce(t, p)
	var status, reason string
	if err := db.QueryRow(`SELECT t.status, json_extract(a.detail, '$.reason') FROM tasks t
		JOIN audit_logs a ON a.target_id = t.id AND a.actor = 'system' AND a.action <> 'task.create'
		WHERE t.id = ?`, got.id).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || reason != string(ReasonLabelRemoved) {
		t.Fatalf("status=%s reason=%s", status, reason)
	}
}
