package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
)

const statusSecret = "lin_api_STATUSSECRET789"

// statusLinear is a fake Linear API that writes the Authorization header it
// receives into every response (Team names, Issue titles and bodies, error
// bodies), so a leak of the API key into GET /api/linear/status shows. With
// failing set it answers 429 (rate limited).
type statusLinear struct {
	mu      sync.Mutex
	issues  map[string]map[string]any
	failing bool
}

func (l *statusLinear) set(id string, issue map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.issues[id] = issue
}

func (l *statusLinear) fail() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failing = true
}

func (l *statusLinear) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	echo := r.Header.Get("Authorization")
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failing {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"errors":[{"message":"slow down ` + echo + `"}]}`))
		return
	}
	var data any
	switch {
	case strings.Contains(req.Query, "EvaloomTeams"):
		data = map[string]any{"teams": map[string]any{"nodes": []any{
			map[string]any{"id": "team-other", "key": "ENG", "name": "Engineering " + echo},
			map[string]any{"id": "team-a", "key": "USLAB", "name": "Lab " + echo},
		}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}
	case strings.Contains(req.Query, "EvaloomReadyIssues"):
		nodes := []any{}
		for _, issue := range l.issues {
			if issue["team"].(map[string]any)["id"] == req.Variables["teamId"] && hasLabel(issue, req.Variables["label"]) {
				nodes = append(nodes, issue)
			}
		}
		data = map[string]any{"issues": map[string]any{"nodes": nodes,
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}
	case strings.Contains(req.Query, "EvaloomIssue"):
		issue, ok := l.issues[req.Variables["id"].(string)]
		if !ok {
			data = map[string]any{"issue": nil}
		} else {
			data = map[string]any{"issue": issue}
		}
	default:
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func statusIssue(id, identifier string, updated time.Time, labels ...string) map[string]any {
	issue := linearIssueJSON(id, "no criteria", updated, labels...)
	issue["identifier"] = identifier
	issue["title"] = "Fix " + statusSecret
	return issue
}

type linearStatusResponse struct {
	Teams []struct {
		TeamID              string  `json:"teamId"`
		LastSuccessAt       *string `json:"lastSuccessAt"`
		LastError           *string `json:"lastError"`
		ConsecutiveFailures int     `json:"consecutiveFailures"`
	} `json:"teams"`
	TeamProblem *struct {
		Reason string `json:"reason"`
	} `json:"teamProblem"`
	Issues []struct {
		LinearIssueID   string  `json:"linearIssueId"`
		IssueIdentifier string  `json:"issueIdentifier"`
		TeamID          string  `json:"teamId"`
		LastDecision    *string `json:"lastDecision"`
		LastDecisionAt  *string `json:"lastDecisionAt"`
	} `json:"issues"`
}

// readLinearStatus calls GET /api/linear/status and checks that neither the
// API key nor anything Linear echoed is in the response (spec 15 3.8, 6章).
func readLinearStatus(t *testing.T, d *daemon) (linearStatusResponse, string) {
	t.Helper()
	status, body := d.request(t, "GET", "/api/linear/status", "")
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	for _, leaked := range []string{statusSecret, "Bearer", "slow down", "Fix ", "Lab ", "Engineering"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("GET /api/linear/status contains %q: %s", leaked, body)
		}
	}
	var view linearStatusResponse
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("body=%s err=%v", body, err)
	}
	return view, body
}

func decisionOf(view linearStatusResponse) map[string]string {
	got := map[string]string{}
	for _, issue := range view.Issues {
		decision := "<null>"
		if issue.LastDecision != nil {
			decision = *issue.LastDecision
			if issue.LastDecisionAt == nil {
				decision += " without time"
			}
		}
		got[issue.IssueIdentifier+"/"+issue.TeamID] = decision
	}
	return got
}

// 受け入れ基準（15 5章、3.8、6章）: GET /api/linear/status に、照会先の Team
// （uslab）の最後に成功した時刻と失敗の分類・続けて失敗した回数、ai-ready が
// 付いている Issue と最後の判定が返る。応答に API キーの値が含まれない。
// evaloomd の本番の配線（linearPoller、実の Linear クライアント）で確かめる。
func TestLinearStatusThroughProductionPoller(t *testing.T) {
	fake := &statusLinear{issues: map[string]map[string]any{}}
	at := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	// Attached before the first poll: recorded only (3.4, Q64).
	fake.set("issue-0", statusIssue("issue-0", "USL-0", at, "ai-ready"))
	server := httptest.NewTLSServer(fake)
	t.Cleanup(server.Close)
	keyPath := filepath.Join(t.TempDir(), "linear-api-key")
	if err := os.WriteFile(keyPath, []byte(statusSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemonWithPoller(t, newLinearDB(t), func(db *sql.DB, logger *slog.Logger) *linearintake.Poller {
		client, err := linear.NewClient(server.Client(), server.URL+"/graphql", keyPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		poller := linearPoller(db, client, logger)
		poller.Interval = time.Hour // only the startup poll runs by itself
		return poller
	})

	// Startup poll: team-a (key USLAB) succeeds.
	deadline := time.Now().Add(5 * time.Second)
	var view linearStatusResponse
	var body string
	for {
		view, body = readLinearStatus(t, d)
		if len(view.Teams) == 1 && view.Teams[0].LastSuccessAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup poll did not succeed: %s", body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	team := view.Teams[0]
	if team.TeamID != "team-a" || team.LastError != nil || team.ConsecutiveFailures != 0 || view.TeamProblem != nil {
		t.Fatalf("after success: %s", body)
	}
	firstSuccess := *team.LastSuccessAt
	if _, err := time.Parse(time.RFC3339Nano, firstSuccess); err != nil {
		t.Fatalf("lastSuccessAt %q: %v", firstSuccess, err)
	}
	if got := decisionOf(view); len(got) != 1 || got["USL-0/team-a"] != "skipped_initial" {
		t.Fatalf("issues after the first poll = %v (%s)", got, body)
	}

	// An Issue attached later without acceptance criteria is held (3.4).
	fake.set("issue-1", statusIssue("issue-1", "USL-1", at.Add(time.Minute), "ai-ready", "route:p"))
	// An Issue without ai-ready is not listed.
	fake.set("issue-2", statusIssue("issue-2", "USL-2", at.Add(time.Minute), "route:p"))
	if _, err := d.poller.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	view, body = readLinearStatus(t, d)
	if got := decisionOf(view); len(got) != 2 || got["USL-0/team-a"] != "skipped_initial" || got["USL-1/team-a"] != "held" {
		t.Fatalf("issues after the second poll = %v (%s)", got, body)
	}

	// Two rate-limited polls: the classification and the count show, and the
	// last success time stays.
	fake.fail()
	for i := 0; i < 2; i++ {
		d.poller.PollOnce(context.Background())
	}
	view, body = readLinearStatus(t, d)
	if len(view.Teams) != 1 {
		t.Fatalf("teams after failures: %s", body)
	}
	team = view.Teams[0]
	if team.TeamID != "team-a" || team.LastError == nil || *team.LastError != "rate_limited" ||
		team.ConsecutiveFailures != 2 || team.LastSuccessAt == nil || *team.LastSuccessAt < firstSuccess {
		t.Fatalf("after failures: %s", body)
	}
	// The Issues and their last decisions are still served.
	if got := decisionOf(view); len(got) != 2 || got["USL-1/team-a"] != "held" {
		t.Fatalf("issues after failures = %v (%s)", got, body)
	}
}
