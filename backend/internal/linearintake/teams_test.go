package linearintake

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linear"
)

func TestSelectPollTeamMatchesUslabKeyOrNameIgnoringCase(t *testing.T) {
	other := linear.Team{ID: "eng", Key: "ENG", Name: "Engineering"}
	for _, test := range []struct {
		name    string
		teams   []linear.Team
		id      string
		problem TeamProblem
	}{
		{"key upper case", []linear.Team{other, {ID: "t1", Key: "USLAB", Name: "Lab"}}, "t1", ""},
		{"name mixed case", []linear.Team{{ID: "t2", Key: "LAB", Name: "UsLab"}, other}, "t2", ""},
		{"key and name of one Team", []linear.Team{{ID: "t3", Key: "uslab", Name: "uslab"}}, "t3", ""},
		{"partial match is not a match", []linear.Team{{ID: "t4", Key: "USLAB2", Name: "uslab team"}}, "", TeamNotFound},
		{"none", []linear.Team{other}, "", TeamNotFound},
		{"empty list", nil, "", TeamNotFound},
		{"two Teams", []linear.Team{{ID: "a", Key: "USLAB", Name: "A"}, {ID: "b", Key: "B", Name: "uslab"}}, "", TeamMultiple},
	} {
		t.Run(test.name, func(t *testing.T) {
			id, problem := SelectPollTeam(test.teams)
			if id != test.id || problem != test.problem {
				t.Fatalf("SelectPollTeam = %q, %q; want %q, %q", id, problem, test.id, test.problem)
			}
		})
	}
	if TeamNotFound.Message() != "照会先 Team（uslab）が見つからない" || TeamMultiple.Message() != "照会先 Team（uslab）が複数ある" {
		t.Fatal("problem messages differ from spec 15 3.2")
	}
}

// teamClient is a stubClient that also lists Teams, so the Poller resolves
// the polling Team itself (the production path, Q216). log records the
// order of Team list reads and issue queries.
type teamClient struct {
	*stubClient
	mu    sync.Mutex
	teams []linear.Team
	err   error
	log   []string
}

func (c *teamClient) ListTeams(context.Context) ([]linear.Team, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, "teams")
	return append([]linear.Team(nil), c.teams...), c.err
}

func (c *teamClient) ListReadyIssues(ctx context.Context, teamID, label string) ([]linear.Issue, error) {
	c.mu.Lock()
	c.log = append(c.log, "issues:"+teamID)
	c.mu.Unlock()
	return c.stubClient.ListReadyIssues(ctx, teamID, label)
}

func (c *teamClient) set(teams []linear.Team, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.teams, c.err = teams, err
}

func (c *teamClient) calls() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.log, ",")
}

func uslabPoller(t *testing.T, client *teamClient) (*Poller, *PollTeamStatus) {
	t.Helper()
	status := &PollTeamStatus{}
	p := newPoller(openDB(t), client, nil)
	p.Teams = nil // resolve from Linear like evaloomd
	p.TeamStatus = status
	return p, status
}

var uslab = linear.Team{ID: "uslab-id", Key: "USLAB", Name: "US Lab"}

// 受け入れ基準: uslab の Team がちょうど1つあればその ID で照会する; Team ID は
// 起動時に1回だけ引き、照会が成功している間は引き直さない。
func TestPollerResolvesUslabOnceWhileQueriesSucceed(t *testing.T) {
	client := &teamClient{stubClient: &stubClient{}, teams: []linear.Team{{ID: "eng", Key: "ENG", Name: "Eng"}, uslab}}
	p, status := uslabPoller(t, client)
	for range 3 {
		if result := pollOnce(t, p); !result.Polled {
			t.Fatal("round did not poll")
		}
	}
	if got := client.calls(); got != "teams,issues:uslab-id,issues:uslab-id,issues:uslab-id" {
		t.Fatalf("calls = %s", got)
	}
	if status.PollTeamProblem() != "" {
		t.Fatalf("problem = %q", status.PollTeamProblem())
	}
	if row := readTeam(t, p.DB, "uslab-id"); !row.lastSuccess.Valid || row.failures != 0 {
		t.Fatalf("uslab row = %+v", row)
	}
}

// 受け入れ基準: 照会がエラーになった後の次の照会の前に Team ID を引き直す。
func TestPollerRereadsTeamAfterFailedQuery(t *testing.T) {
	failing := true
	stub := &stubClient{list: func(string) ([]linear.Issue, error) {
		if failing {
			return nil, &linear.Error{Op: "list issues", Class: linear.ClassNetwork}
		}
		return nil, nil
	}}
	client := &teamClient{stubClient: stub, teams: []linear.Team{uslab}}
	p, _ := uslabPoller(t, client)
	pollOnce(t, p) // resolve, query fails
	if row := readTeam(t, p.DB, "uslab-id"); row.lastError.String != "network" || row.failures != 1 {
		t.Fatalf("row after failure = %+v", row)
	}
	// The uslab Team was recreated meanwhile: the re-read finds the new ID.
	recreated := linear.Team{ID: "uslab-new", Key: "uslab", Name: "US Lab"}
	client.set([]linear.Team{recreated}, nil)
	failing = false
	pollOnce(t, p) // re-read, query succeeds
	pollOnce(t, p) // no re-read
	if got := client.calls(); got != "teams,issues:uslab-id,teams,issues:uslab-new,issues:uslab-new" {
		t.Fatalf("calls = %s", got)
	}
}

// An unusable key file is a failed query too: the next round reads the Team
// list again (spec 15 6章, Q216).
func TestPollerRereadsTeamAfterKeyFailure(t *testing.T) {
	stub := &stubClient{keyErr: &linear.KeyError{Problem: linear.KeyPermission}}
	client := &teamClient{stubClient: stub, teams: []linear.Team{uslab}}
	p, _ := uslabPoller(t, client)
	pollOnce(t, p)
	if row := readTeam(t, p.DB, "uslab-id"); row.lastError.String != "auth" {
		t.Fatalf("row = %+v; want auth", row)
	}
	stub.mu.Lock()
	stub.keyErr = nil
	stub.mu.Unlock()
	pollOnce(t, p)
	if got := client.calls(); got != "teams,teams,issues:uslab-id" {
		t.Fatalf("calls = %s", got)
	}
}

// 受け入れ基準: uslab の Team が見つからない・複数あるときは照会せず、
// 「照会先 Team（uslab）が見つからない」「照会先 Team（uslab）が複数ある」が出る。
func TestPollerDoesNotQueryWithoutSingleUslabTeam(t *testing.T) {
	for _, test := range []struct {
		name    string
		teams   []linear.Team
		problem TeamProblem
		message string
	}{
		{"not found", []linear.Team{{ID: "eng", Key: "ENG", Name: "Eng"}}, TeamNotFound, "照会先 Team（uslab）が見つからない"},
		{"multiple", []linear.Team{uslab, {ID: "other", Key: "OTH", Name: "USLAB"}}, TeamMultiple, "照会先 Team（uslab）が複数ある"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &teamClient{stubClient: &stubClient{}, teams: []linear.Team{uslab}}
			p, status := uslabPoller(t, client)
			pollOnce(t, p) // a previous run polled uslab-id
			before := readTeam(t, p.DB, "uslab-id")

			client.set(test.teams, nil)
			p.Teams = NewUslabTeams(client) // evaloomd restarted: resolve again
			for range 2 {
				if result := pollOnce(t, p); result.Polled {
					t.Fatal("round polled without a single uslab Team")
				}
			}
			if got := client.calls(); got != "teams,issues:uslab-id,teams,teams" {
				t.Fatalf("calls = %s; want no query and a re-read each round", got)
			}
			if _, _, keyCalls := client.snapshot(); keyCalls != 1 {
				t.Fatalf("key checked %d times; want only the first round", keyCalls)
			}
			if status.PollTeamProblem() != test.problem || status.PollTeamProblem().Message() != test.message {
				t.Fatalf("problem = %q (%s)", status.PollTeamProblem(), status.PollTeamProblem().Message())
			}
			// Intake stops; the known Team's row is left as it was.
			if after := readTeam(t, p.DB, "uslab-id"); after != before {
				t.Fatalf("row changed: %+v -> %+v", before, after)
			}

			// Once a single uslab Team exists again, polling resumes and the
			// problem is cleared.
			client.set([]linear.Team{uslab}, nil)
			if result := pollOnce(t, p); !result.Polled {
				t.Fatal("round did not poll after the Team came back")
			}
			if status.PollTeamProblem() != "" {
				t.Fatalf("problem = %q after resolving", status.PollTeamProblem())
			}
		})
	}
}

// A failed Team list read is a failed query of the known polling Team: its
// last_error and consecutive_failures change, and the next round reads the
// list again (spec 15 6章).
func TestPollerRecordsFailedTeamListOnKnownTeam(t *testing.T) {
	client := &teamClient{stubClient: &stubClient{}, teams: []linear.Team{uslab}}
	p, status := uslabPoller(t, client)
	pollOnce(t, p)
	p.Teams = NewUslabTeams(client) // restarted
	client.set(nil, &linear.Error{Op: "list teams", Class: linear.ClassNetwork})
	if result := pollOnce(t, p); result.Polled {
		t.Fatal("round polled without a Team list")
	}
	if row := readTeam(t, p.DB, "uslab-id"); row.lastError.String != "network" || row.failures != 1 {
		t.Fatalf("row = %+v", row)
	}
	if status.PollTeamProblem() != "" {
		t.Fatalf("problem = %q; a failed read is not not_found", status.PollTeamProblem())
	}
	client.set([]linear.Team{uslab}, nil)
	pollOnce(t, p)
	if got := client.calls(); got != "teams,issues:uslab-id,teams,teams,issues:uslab-id" {
		t.Fatalf("calls = %s", got)
	}
	if row := readTeam(t, p.DB, "uslab-id"); row.lastError.Valid || row.failures != 0 {
		t.Fatalf("row after success = %+v", row)
	}
}

// A database error from the TeamSource is returned, not recorded as a
// Linear failure.
func TestPollerReturnsTeamSourceDatabaseError(t *testing.T) {
	p := newPoller(openDB(t), &stubClient{}, nil)
	want := errors.New("db down")
	p.Teams = errTeams{want}
	if _, err := p.PollOnce(context.Background()); !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
}

type errTeams struct{ err error }

func (e errTeams) LinearPollTeamID(context.Context) (string, error) { return "", e.err }

// On a first start with an unusable key no Team is known yet: nothing is
// queried, no linear_poll_teams row is invented, and the authentication
// error alerts once through the shared suppression (spec 15 6章, Q84).
func TestPollerAlertsAuthOnceWhenTeamListNeedsKeyOnFirstStart(t *testing.T) {
	client := &teamClient{stubClient: &stubClient{}, err: &linear.KeyError{Problem: linear.KeyMissing}}
	p, _ := uslabPoller(t, client)
	alerter := &recordingAlerter{}
	p.Alerter = alerter
	for range 2 {
		if result := pollOnce(t, p); result.Polled {
			t.Fatal("round polled without a Team")
		}
	}
	if got := client.calls(); got != "teams,teams" {
		t.Fatalf("calls = %s", got)
	}
	var rows int
	if err := p.DB.QueryRow(`SELECT count(*) FROM linear_poll_teams`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("linear_poll_teams rows = %d err=%v", rows, err)
	}
	if alerter.count() != 1 || alerter.alerts[0].Class != linear.ClassAuth {
		t.Fatalf("alerts = %+v; want one auth alert", alerter.alerts)
	}
}
