package linearintake

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
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
	// A proposer model keeps route proposals queued for a Runner; the
	// default null model fails them at once (spec 15 3.6.4 step 2).
	setProposerModel(t, db, `"model-x"`)
	return db
}

// setProposerModel writes routing.proposer with the given model JSON.
func setProposerModel(t *testing.T, db *sql.DB, model string) {
	t.Helper()
	exec(t, db, `INSERT INTO settings (key, value, updated_at) VALUES ('routing.proposer', ?, '2026-11-08T00:00:00Z')
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, `{"microHarness":"claude_code","model":`+model+`}`)
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// testTeams is the polling Team of each test database. In production the
// Team is resolved from Linear (uslab, Q216); these tests name it directly.
var testTeams sync.Map // *sql.DB -> string

// setTestTeam makes teamID the polling Team of db.
func setTestTeam(db *sql.DB, teamID string) { testTeams.Store(db, teamID) }

// dbTeams is a TeamSource naming db's test polling Team.
type dbTeams struct{ db *sql.DB }

func (d dbTeams) LinearPollTeamID(context.Context) (string, error) {
	id, _ := testTeams.Load(d.db)
	team, _ := id.(string)
	return team, nil
}

// syncTestTeams returns db's polling Team row as the Poller sees it.
func syncTestTeams(ctx context.Context, db *sql.DB) ([]store.LinearPollTeam, error) {
	id, _ := dbTeams{db}.LinearPollTeamID(ctx)
	return store.SyncLinearPollTeam(ctx, db, id)
}

// addProject registers a direct Profile pin and, optionally, a route. The
// first non-empty Team becomes the polling Team.
func addProject(t *testing.T, db *sql.DB, id string, teamID any, label any) {
	t.Helper()
	exec(t, db, `INSERT INTO projects (id,name,harness_id,profile_id,runner_id,created_at,updated_at) VALUES (?,'Project','fixture-harness','fixture-profile','r1','t','t')`, id)
	if team, ok := teamID.(string); ok && team != "" {
		testTeams.LoadOrStore(db, team)
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
	return &Poller{DB: db, Client: client, Teams: dbTeams{db}, Logger: slog.New(slog.NewTextHandler(logs, nil))}
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

// fakeClock drives Run without sleeping. Now returns the fake time; each
// wait Run asks for is handed to the test, which advances the time and fires
// it.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan fakeWait
}

type fakeWait struct {
	d    time.Duration
	fire chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), waits: make(chan fakeWait, 16)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	fire := make(chan time.Time, 1)
	c.waits <- fakeWait{d: d, fire: fire}
	return fire
}

// nextWait returns the next wait Run asks for.
func (c *fakeClock) nextWait(t *testing.T) fakeWait {
	t.Helper()
	select {
	case w := <-c.waits:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not wait for the next round")
		return fakeWait{}
	}
}

// elapse lets the wait's whole duration pass and fires it.
func (c *fakeClock) elapse(w fakeWait) {
	c.Advance(w.d)
	w.fire <- c.Now()
}

// startRun runs p.Run on the fake clock and returns a stop function.
func startRun(t *testing.T, p *Poller, clock *fakeClock) (stop func()) {
	t.Helper()
	p.Now, p.After = clock.Now, clock.After
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	return func() { cancel(); <-done }
}

// 受け入れ基準: linear_poll_teams の last_error と consecutive_failures が失敗で
// 更新され、成功で戻る (spec 15 6章, 3.8).
func TestPollFailureIsRecordedAndSuccessClearsIt(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "route:one")
	var next atomic.Value
	next.Store(linear.ClassNetwork)
	client := &stubClient{list: func(string) ([]linear.Issue, error) {
		class := next.Load().(linear.ErrorClass)
		if class == "" {
			return nil, nil
		}
		return nil, &linear.Error{Op: "list issues", Class: class}
	}}
	clock := newFakeClock()
	p := newPoller(db, client, nil)
	p.Now = clock.Now

	pollOnce(t, p)
	if row := readTeam(t, db, "team-a"); row.lastError.String != "network" || row.failures != 1 || row.lastSuccess.Valid {
		t.Fatalf("after first failure: %+v", row)
	}
	next.Store(linear.ClassOther)
	pollOnce(t, p)
	if row := readTeam(t, db, "team-a"); row.lastError.String != "other" || row.failures != 2 {
		t.Fatalf("after second failure: %+v", row)
	}
	next.Store(linear.ErrorClass(""))
	pollOnce(t, p)
	row := readTeam(t, db, "team-a")
	if row.lastError.Valid || row.failures != 0 || !row.lastSuccess.Valid {
		t.Fatalf("after success: %+v", row)
	}
	if got, err := time.Parse(time.RFC3339Nano, row.lastSuccess.String); err != nil || !got.Equal(clock.Now()) {
		t.Fatalf("last_success_at = %q (%v); want %v", row.lastSuccess.String, err, clock.Now())
	}
}

// 受け入れ基準: API キーのファイルが無い・権限が 0600 でないとき、照会先の
// Team の last_error が auth になる。照会は始めない (spec 15 6章).
func TestUnusableKeyFileMarksPollTeamAuthWithoutQuery(t *testing.T) {
	for _, problem := range []linear.KeyProblem{linear.KeyMissing, linear.KeyPermission} {
		t.Run(string(problem), func(t *testing.T) {
			db := openDB(t)
			addProject(t, db, "p1", "team-a", "route:one")
			client := &stubClient{keyErr: &linear.KeyError{Problem: problem}}
			alerter := &recordingAlerter{}
			p := newPoller(db, client, nil)
			p.Alerter = alerter
			pollOnce(t, p)
			if calls, _, keyCalls := client.snapshot(); len(calls) != 0 || keyCalls != 1 {
				t.Fatalf("calls=%v keyCalls=%d; want no query after one key check", calls, keyCalls)
			}
			if row := readTeam(t, db, "team-a"); row.lastError.String != "auth" || row.failures != 1 {
				t.Fatalf("team-a = %+v; want last_error auth", row)
			}
			if alerter.count() != 1 || alerter.alerts[0].Class != linear.ClassAuth {
				t.Fatalf("alerts = %+v; want one auth alert", alerter.alerts)
			}
		})
	}
}

type fixedTeams struct{ id string }

func (f fixedTeams) LinearPollTeamID(context.Context) (string, error) { return f.id, nil }

// 受け入れ基準: 照会先 Team が未設定（Team を得る口が空を返す）のときは照会しない。
// Team を決めるのは口の側で、Poller は受け取った Team だけを照会する。
func TestPollerQueriesOnlyTheTeamItsSourceNames(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "route:one") // polling Team = team-a
	client := &stubClient{}
	p := newPoller(db, client, nil)

	p.Teams = fixedTeams{""}
	if result := pollOnce(t, p); result.Polled {
		t.Fatal("round reported as polled with no polling Team")
	}
	if calls, _, keyCalls := client.snapshot(); len(calls) != 0 || keyCalls != 0 {
		t.Fatalf("queried with no polling Team: calls=%v keyCalls=%d", calls, keyCalls)
	}

	p.Teams = fixedTeams{"team-x"}
	if result := pollOnce(t, p); !result.Polled {
		t.Fatal("round with a polling Team reported as not polled")
	}
	if calls, _, _ := client.snapshot(); strings.Join(calls, ",") != "team-x/ai-ready" {
		t.Fatalf("calls = %v; want only team-x", calls)
	}
	if row := readTeam(t, db, "team-x"); row.failures != 0 || !row.lastSuccess.Valid {
		t.Fatalf("team-x = %+v", row)
	}
}

func TestRateLimitStopsRoundUntilResetHint(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "route:one")
	clock := newFakeClock()
	const interval = time.Minute
	var limited atomic.Bool
	limited.Store(true)
	client := &stubClient{list: func(string) ([]linear.Issue, error) {
		if limited.CompareAndSwap(true, false) {
			return nil, &linear.Error{Op: "list issues", Class: linear.ClassRateLimited, ResetAt: clock.Now().Add(5 * interval)}
		}
		return nil, nil
	}}
	p := newPoller(db, client, nil)
	p.Interval = interval
	stop := startRun(t, p, clock)
	defer stop()

	w := clock.nextWait(t)
	if w.d != 5*interval {
		t.Fatalf("wait after rate limit = %v; want Linear's reset hint %v", w.d, 5*interval)
	}
	if row := readTeam(t, db, "team-a"); row.lastError.String != "rate_limited" || row.failures != 1 {
		t.Fatalf("team-a while limited = %+v", row)
	}
	clock.elapse(w)
	if w := clock.nextWait(t); w.d != interval {
		t.Fatalf("wait after recovery = %v; want %v", w.d, interval)
	}
	if calls, _, _ := client.snapshot(); len(calls) != 2 {
		t.Fatalf("calls = %v; want 2", calls)
	}
	if row := readTeam(t, db, "team-a"); row.lastError.Valid || row.failures != 0 {
		t.Fatalf("team-a after recovery = %+v", row)
	}
}

// 受け入れ基準: Control Plane の起動で1回照会し、その後は設定した間隔で照会する。
func TestRunPollsAtStartupThenEachInterval(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "route:one")
	client := &stubClient{}
	clock := newFakeClock()
	p := newPoller(db, client, nil)
	p.Interval = 3 * time.Minute
	stop := startRun(t, p, clock)
	defer stop()

	for round := 1; round <= 3; round++ {
		w := clock.nextWait(t)
		// The round ran before Run waited: the first one at startup.
		if calls, _, _ := client.snapshot(); len(calls) != round {
			t.Fatalf("calls before wait %d = %d; want %d", round, len(calls), round)
		}
		if w.d != p.Interval {
			t.Fatalf("wait %d = %v; want the configured interval %v", round, w.d, p.Interval)
		}
		clock.elapse(w)
	}
}

// 受け入れ基準: 照会が間隔より長く掛かっても、2本目が同時に始まらない。
func TestSlowRoundsNeverOverlap(t *testing.T) {
	db := openDB(t)
	addProject(t, db, "p1", "team-a", "route:one")
	clock := newFakeClock()
	const interval = time.Minute
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var active, overlapped atomic.Int32
	client := &stubClient{list: func(string) ([]linear.Issue, error) {
		if active.Add(1) > 1 {
			overlapped.Add(1)
		}
		entered <- struct{}{}
		<-release
		clock.Advance(3 * interval) // the round outlasts the interval
		active.Add(-1)
		return nil, nil
	}}
	p := newPoller(db, client, nil)
	p.Interval = interval
	stop := startRun(t, p, clock)
	// Direct calls alongside Run must also wait for the running round.
	var wg, launched sync.WaitGroup
	for range 3 {
		wg.Add(1)
		launched.Add(1)
		go func() { defer wg.Done(); launched.Done(); p.PollOnce(context.Background()) }()
	}
	for round := range 4 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d did not start", round+1)
		}
		if round == 0 {
			// While the first round is held, every caller is trying to start
			// one. None may enter before it ends.
			launched.Wait()
			select {
			case <-entered:
				t.Fatal("a second round started while the first was running")
			case <-time.After(200 * time.Millisecond):
			}
		}
		release <- struct{}{}
	}
	wg.Wait()
	// The round outlasted the interval, so the next starts without waiting,
	// but only after Run is let through: never during the previous round.
	if w := clock.nextWait(t); w.d != 0 {
		t.Fatalf("wait after a slow round = %v; want 0", w.d)
	}
	stop()
	if calls, _, _ := client.snapshot(); len(calls) != 4 || overlapped.Load() != 0 {
		t.Fatalf("calls = %d, overlapping rounds = %d; want 4 and 0", len(calls), overlapped.Load())
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
	setTestTeam(db, "team-b")
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
