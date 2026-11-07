package linearintake

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// stubClient stands in for the Linear API; no test reaches the real one.
type stubClient struct {
	mu       sync.Mutex
	keyErr   error
	list     func(teamID string) ([]linear.Issue, error)
	keyCalls int
	calls    []string
	times    []time.Time
}

func (c *stubClient) CheckKey() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keyCalls++
	return c.keyErr
}

func (c *stubClient) ListReadyIssues(_ context.Context, teamID, label string) ([]linear.Issue, error) {
	c.mu.Lock()
	c.calls = append(c.calls, teamID+"/"+label)
	c.times = append(c.times, time.Now())
	list := c.list
	c.mu.Unlock()
	if list == nil {
		return nil, nil
	}
	return list(teamID)
}

func (c *stubClient) snapshot() ([]string, []time.Time, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...), append([]time.Time(nil), c.times...), c.keyCalls
}

type recordingAlerter struct {
	mu     sync.Mutex
	alerts []Alert
}

func (a *recordingAlerter) AlertLinearPollFailing(_ context.Context, alert Alert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts = append(a.alerts, alert)
	return nil
}

func (a *recordingAlerter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.alerts)
}

type recordingHandler struct {
	teams  []string
	issues [][]linear.Issue
}

func (h *recordingHandler) HandleTeamIssues(_ context.Context, team store.LinearPollTeam, issues []linear.Issue) error {
	h.teams = append(h.teams, team.TeamID)
	h.issues = append(h.issues, issues)
	return nil
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	exec(t, db, `INSERT INTO runners (id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
		VALUES ('r1', 'r1', 'h', '/w', 'online', '2026-11-08T00:00:00Z', '2026-11-08T00:00:00Z')`)
	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// addProject inserts a Project registering teamID (nil leaves it NULL).
func addProject(t *testing.T, db *sql.DB, id string, teamID any, label any) {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info('projects') WHERE "notnull" = 1 AND dflt_value IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	var values []any
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, column)
		switch column {
		case "id":
			values = append(values, id)
		case "local_repo_path":
			values = append(values, "/repos/"+id)
		case "runner_id":
			values = append(values, "r1")
		default:
			values = append(values, "x")
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	columns = append(columns, "linear_team_id", "linear_routing_label")
	values = append(values, teamID, label)
	exec(t, db, `INSERT INTO projects (`+strings.Join(columns, ", ")+`) VALUES (?`+strings.Repeat(", ?", len(columns)-1)+`)`, values...)
}

type teamRow struct {
	lastSuccess sql.NullString
	lastError   sql.NullString
	failures    int
}

func readTeam(t *testing.T, db *sql.DB, teamID string) teamRow {
	t.Helper()
	var row teamRow
	if err := db.QueryRow(`SELECT last_success_at, last_error, consecutive_failures FROM linear_poll_teams WHERE team_id = ?`,
		teamID).Scan(&row.lastSuccess, &row.lastError, &row.failures); err != nil {
		t.Fatalf("team %s: %v", teamID, err)
	}
	return row
}

func newPoller(db *sql.DB, client Client, logs *bytes.Buffer) *Poller {
	if logs == nil {
		logs = &bytes.Buffer{}
	}
	return &Poller{DB: db, Client: client, Logger: slog.New(slog.NewTextHandler(logs, nil))}
}

func pollOnce(t *testing.T, p *Poller) Result {
	t.Helper()
	result, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNoRegisteredTeamDoesNotQuery(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p-none", nil, nil)
	addProject(t, db, "p-empty", "", "repo:x")
	// A stale row of a Team no Project registers any more is removed (3.8).
	exec(t, db, `INSERT INTO linear_poll_teams (team_id, consecutive_failures) VALUES ('gone', 3)`)
	client := &stubClient{}
	p := newPoller(db, client, nil)
	if result := pollOnce(t, p); result.Polled {
		t.Fatal("round reported as polled without registered Teams")
	}
	if calls, _, keyCalls := client.snapshot(); len(calls) != 0 || keyCalls != 0 {
		t.Fatalf("queried without registered Teams: calls=%v keyCalls=%d", calls, keyCalls)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM linear_poll_teams`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("linear_poll_teams rows = %d, %v; want 0", count, err)
	}
}

func TestSharedTeamIsQueriedOnceWithTriggerLabel(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "repo:one")
	addProject(t, db, "p2", "team-a", "repo:two")
	addProject(t, db, "p3", "team-b", "repo:one")
	issues := []linear.Issue{{ID: "issue-1", TeamID: "team-a"}, {ID: "issue-2", TeamID: "team-a"}}
	client := &stubClient{list: func(teamID string) ([]linear.Issue, error) {
		if teamID == "team-a" {
			return issues, nil
		}
		return nil, nil
	}}
	handler := &recordingHandler{}
	p := newPoller(db, client, nil)
	p.Handler = handler
	if result := pollOnce(t, p); !result.Polled {
		t.Fatal("round not polled")
	}
	calls, _, _ := client.snapshot()
	if strings.Join(calls, ",") != "team-a/ai-ready,team-b/ai-ready" {
		t.Fatalf("calls = %v", calls)
	}
	if strings.Join(handler.teams, ",") != "team-a,team-b" || len(handler.issues[0]) != 2 {
		t.Fatalf("handler got teams=%v issues=%v", handler.teams, handler.issues)
	}
	for _, team := range []string{"team-a", "team-b"} {
		if row := readTeam(t, db, team); !row.lastSuccess.Valid || row.lastError.Valid || row.failures != 0 {
			t.Fatalf("%s row = %+v", team, row)
		}
	}
}

func TestFailureIsRecordedAndSuccessResetsIt(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "repo:one")
	addProject(t, db, "p2", "team-b", "repo:one")
	var failMu sync.Mutex
	var failErr error
	setFail := func(err error) { failMu.Lock(); failErr = err; failMu.Unlock() }
	client := &stubClient{list: func(teamID string) ([]linear.Issue, error) {
		failMu.Lock()
		err := failErr
		failMu.Unlock()
		if err != nil && teamID == "team-a" {
			return nil, err
		}
		return []linear.Issue{{ID: "i", TeamID: teamID}}, nil
	}}
	handler := &recordingHandler{}
	p := newPoller(db, client, nil)
	p.Handler = handler

	pollOnce(t, p)
	success := readTeam(t, db, "team-a").lastSuccess
	setFail(&linear.Error{Op: "list issues", Class: linear.ClassNetwork})
	pollOnce(t, p)
	if row := readTeam(t, db, "team-a"); row.lastError.String != "network" || row.failures != 1 || row.lastSuccess != success {
		t.Fatalf("after network failure: %+v", row)
	}
	setFail(errors.New("unclassified"))
	pollOnce(t, p)
	if row := readTeam(t, db, "team-a"); row.lastError.String != "other" || row.failures != 2 {
		t.Fatalf("after other failure: %+v", row)
	}
	// Another Team's success is recorded independently.
	if row := readTeam(t, db, "team-b"); row.lastError.Valid || row.failures != 0 {
		t.Fatalf("team-b affected by team-a failure: %+v", row)
	}
	// The failed Team's list never reaches the handler (spec 15 3.2).
	if got := strings.Join(handler.teams, ","); got != "team-a,team-b,team-b,team-b" {
		t.Fatalf("handler teams = %s", got)
	}
	setFail(nil)
	time.Sleep(time.Millisecond)
	pollOnce(t, p)
	if row := readTeam(t, db, "team-a"); row.lastError.Valid || row.failures != 0 || !row.lastSuccess.Valid || row.lastSuccess == success {
		t.Fatalf("after recovery: %+v", row)
	}
}

func TestFiveConsecutiveFailuresAlertOncePerStreak(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "repo:one")
	addProject(t, db, "p2", "team-b", "repo:one")
	var failing atomic.Bool
	failing.Store(true)
	client := &stubClient{list: func(string) ([]linear.Issue, error) {
		if failing.Load() {
			return nil, &linear.Error{Op: "list issues", Class: linear.ClassNetwork}
		}
		return nil, nil
	}}
	alerter := &recordingAlerter{}
	p := newPoller(db, client, nil)
	p.Alerter = alerter
	for round := 1; round <= 7; round++ {
		pollOnce(t, p)
		want := 0
		if round >= FailureAlertThreshold {
			want = 1 // both Teams cross the threshold together: one notification
		}
		if got := alerter.count(); got != want {
			t.Fatalf("round %d: alerts = %d; want %d", round, got, want)
		}
	}
	if alert := alerter.alerts[0]; alert.Class != linear.ClassNetwork || alert.ConsecutiveFailures != FailureAlertThreshold {
		t.Fatalf("alert = %+v", alert)
	}
	// The count keeps growing for Dashboard's error status (spec 15 6章).
	if row := readTeam(t, db, "team-a"); row.failures != 7 || row.lastError.String != "network" {
		t.Fatalf("team-a row = %+v", row)
	}
	// A success ends the streak; a new streak alerts again.
	failing.Store(false)
	pollOnce(t, p)
	failing.Store(true)
	for range FailureAlertThreshold {
		pollOnce(t, p)
	}
	if got := alerter.count(); got != 2 {
		t.Fatalf("alerts after second streak = %d; want 2", got)
	}
}

func TestKeyFileProblemsMarkEveryTeamAuthWithoutQuerying(t *testing.T) {
	const canary = "lin_api_EVALOOM_IAO4_FAKE_KEY_91ab"
	cases := map[string]func(t *testing.T) string{
		"missing": func(t *testing.T) string { return filepath.Join(t.TempDir(), linear.APIKeyFileName) },
		"0644": func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), linear.APIKeyFileName)
			if err := os.WriteFile(path, []byte(canary), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
			return path
		},
	}
	for name, keyPath := range cases {
		t.Run(name, func(t *testing.T) {
			db := openDB(t)
			addProject(t, db, "p1", "team-a", "repo:one")
			addProject(t, db, "p2", "team-b", "repo:one")
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			defer server.Close()
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			client, err := linear.NewClient(server.Client(), server.URL, keyPath(t), logger)
			if err != nil {
				t.Fatal(err)
			}
			alerter := &recordingAlerter{}
			p := &Poller{DB: db, Client: client, Logger: logger, Alerter: alerter}
			for round := 1; round <= FailureAlertThreshold+1; round++ {
				pollOnce(t, p)
			}
			if requests.Load() != 0 {
				t.Fatalf("Linear queried with an unusable key file: %d requests", requests.Load())
			}
			for _, team := range []string{"team-a", "team-b"} {
				if row := readTeam(t, db, team); row.lastError.String != "auth" || row.failures != FailureAlertThreshold+1 {
					t.Fatalf("%s row = %+v", team, row)
				}
			}
			// Auth alerts as soon as it starts, once for the whole streak.
			if alerter.count() != 1 || alerter.alerts[0].Class != linear.ClassAuth {
				t.Fatalf("alerts = %+v", alerter.alerts)
			}
			if !strings.Contains(logs.String(), "reason=api_key_") || strings.Contains(logs.String(), canary) {
				t.Fatalf("reason missing or key leaked in logs: %q", logs.String())
			}
		})
	}
}

func TestRateLimitStopsRoundUntilResetHint(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "repo:one")
	addProject(t, db, "p2", "team-b", "repo:one")
	const wait = 400 * time.Millisecond
	var limited atomic.Bool
	limited.Store(true)
	var resetAt time.Time
	client := &stubClient{list: func(teamID string) ([]linear.Issue, error) {
		if teamID == "team-a" && limited.CompareAndSwap(true, false) {
			resetAt = time.Now().Add(wait)
			return nil, &linear.Error{Op: "list issues", Class: linear.ClassRateLimited, ResetAt: resetAt}
		}
		return nil, nil
	}}
	p := newPoller(db, client, nil)
	p.Interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls, _, _ := client.snapshot()
		if len(calls) >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	calls, times, _ := client.snapshot()
	if len(calls) < 3 || calls[0] != "team-a/ai-ready" || calls[1] != "team-a/ai-ready" {
		// team-b must not be queried in the rate-limited round.
		t.Fatalf("calls = %v", calls)
	}
	if times[1].Before(resetAt) {
		t.Fatalf("polling resumed %v before Linear's reset hint", resetAt.Sub(times[1]))
	}
	if row := readTeam(t, db, "team-a"); row.lastError.Valid || row.failures != 0 {
		t.Fatalf("team-a after recovery = %+v", row)
	}
}

func TestRateLimitIsRecordedAndReturnsResumeAt(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "repo:one")
	addProject(t, db, "p2", "team-b", "repo:one")
	resetAt := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	client := &stubClient{list: func(string) ([]linear.Issue, error) {
		return nil, &linear.Error{Op: "list issues", Class: linear.ClassRateLimited, ResetAt: resetAt}
	}}
	result := pollOnce(t, newPoller(db, client, nil))
	if !result.ResumeAt.Equal(resetAt) {
		t.Fatalf("ResumeAt = %v; want %v", result.ResumeAt, resetAt)
	}
	if row := readTeam(t, db, "team-a"); row.lastError.String != "rate_limited" || row.failures != 1 {
		t.Fatalf("team-a = %+v", row)
	}
	if row := readTeam(t, db, "team-b"); row.lastError.Valid || row.failures != 0 {
		t.Fatalf("team-b changed although not queried: %+v", row)
	}
}

func TestRunPollsAtStartupThenEachInterval(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "repo:one")
	client := &stubClient{}
	p := newPoller(db, client, nil)
	p.Interval = 150 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	time.Sleep(p.Interval / 2)
	if calls, _, _ := client.snapshot(); len(calls) != 1 {
		t.Fatalf("calls before the first interval = %d; want 1 (startup poll)", len(calls))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if calls, _, _ := client.snapshot(); len(calls) >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	_, times, _ := client.snapshot()
	if len(times) < 3 || times[0].Sub(started) > p.Interval/2 {
		t.Fatalf("poll times = %v", times)
	}
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < p.Interval-10*time.Millisecond {
			t.Fatalf("gap %d = %v; want about %v", i, gap, p.Interval)
		}
	}
}

func TestSlowRoundsNeverOverlap(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "repo:one")
	var active, maxActive atomic.Int32
	client := &stubClient{list: func(string) ([]linear.Issue, error) {
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond) // longer than the interval
		active.Add(-1)
		return nil, nil
	}}
	p := newPoller(db, client, nil)
	p.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	// A direct call alongside Run must also wait for the running round.
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); p.PollOnce(ctx) }()
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	wg.Wait()
	if calls, _, _ := client.snapshot(); len(calls) < 3 || maxActive.Load() != 1 {
		t.Fatalf("calls = %d, max concurrent rounds = %d; want 1", len(calls), maxActive.Load())
	}
}

func TestShutdownDuringQueryIsNotRecordedAsFailure(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "repo:one")
	ctx, cancel := context.WithCancel(context.Background())
	client := &stubClient{list: func(string) ([]linear.Issue, error) {
		cancel()
		return nil, &linear.Error{Op: "list issues", Class: linear.ClassNetwork}
	}}
	if _, err := newPoller(db, client, nil).PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if row := readTeam(t, db, "team-a"); row.lastError.Valid || row.failures != 0 {
		t.Fatalf("cancelled round recorded: %+v", row)
	}
}
