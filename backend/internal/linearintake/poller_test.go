package linearintake

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/testregistry"
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
	db, err := testregistry.Open(context.Background(), filepath.Join(t.TempDir(), "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	exec(t, db, `INSERT INTO runners (id, name, token_hash, status, last_heartbeat_at, created_at) VALUES ('r1', 'r1', 'h', 'online', '2026-11-08T00:00:00Z', '2026-11-08T00:00:00Z')`)
	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// addProject registers a direct Profile pin and, optionally, a route. The
// polling Team is a global setting; the first non-empty Team configures it.
func addProject(t *testing.T, db *sql.DB, id string, teamID any, label any) {
	t.Helper()
	exec(t, db, `INSERT INTO projects (id,name,harness_id,profile_id,runner_id,created_at,updated_at) VALUES (?,'Project','fixture-harness','fixture-profile','r1','t','t')`, id)
	if team, ok := teamID.(string); ok && team != "" {
		exec(t, db, `INSERT OR IGNORE INTO settings VALUES ('linear.teamId',?,'t')`, strconv.Quote(team))
	}
	if label, ok := label.(string); ok && label != "" {
		exec(t, db, `INSERT INTO routes VALUES (?, ?, NULL,'t','t')`, label, id)
	}
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
	addProject(t, db, "p-empty", "", "route:x")
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

func TestFiveConsecutiveFailuresAlertOncePerStreak(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "route:one")

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

func TestRateLimitStopsRoundUntilResetHint(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "route:one")

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

func TestRunPollsAtStartupThenEachInterval(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "route:one")
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
	addProject(t, db, "p1", "team-a", "route:one")
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
	addProject(t, db, "p1", "team-a", "route:one")
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
func TestGlobalTeamIsQueriedOnceRegardlessOfProjectCount(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "route:one")
	addProject(t, db, "p2", "team-a", "route:two")
	client := &stubClient{}
	p := newPoller(db, client, nil)
	pollOnce(t, p)
	calls, _, _ := client.snapshot()
	if strings.Join(calls, ",") != "team-a/ai-ready" {
		t.Fatalf("calls=%v", calls)
	}
	// Projects do not determine the polling Team.
	exec(t, db, `UPDATE settings SET value='"team-b"' WHERE key='linear.teamId'`)
	pollOnce(t, p)
	calls, _, _ = client.snapshot()
	if strings.Join(calls, ",") != "team-a/ai-ready,team-b/ai-ready" {
		t.Fatal(calls)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM linear_poll_teams WHERE team_id='team-a'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
