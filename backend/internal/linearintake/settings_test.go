package linearintake

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
)

func TestLabelSettingsStartAtDefaults(t *testing.T) {
	if got := NewLabelSettings().LinearLabels(); got != (Labels{TriggerLabel: "ai-ready", RoutingLabelPrefix: "repo:"}) {
		t.Fatalf("defaults = %+v", got)
	}
	if got := roundLabels(context.Background()); got != DefaultLabels() {
		t.Fatalf("round labels without a round = %+v", got)
	}
}

// The Poller reads the global settings linear.triggerLabel and
// linear.routingLabelPrefix (spec 15 3.4, 5章, Q48) and the whole round, up
// to TaskStarter's routing and Intake's detach check, uses them.
func TestPollerUsesGlobalLabelSettings(t *testing.T) {
	db := startDB(t)
	exec(t, db, `UPDATE projects SET linear_routing_label = 'to:one' WHERE id = 'p1'`)
	settings := NewLabelSettings()
	settings.SetLinearLabels(Labels{TriggerLabel: "go", RoutingLabelPrefix: "to:"})

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
