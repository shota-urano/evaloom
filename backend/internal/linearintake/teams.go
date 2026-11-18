package linearintake

import (
	"context"
	"strings"
	"sync"

	"github.com/shota-urano/evaloom/backend/internal/linear"
)

// PollTeamName is the key or name of the polling Team, fixed by the owner's
// decision of 2026-11-18 (spec 15 3.2, 4章, Q216). It is not a setting.
const PollTeamName = "uslab"

// TeamProblem says why no polling Team could be chosen (spec 15 3.2, 6章).
type TeamProblem string

const (
	// TeamNotFound: no Team has the key or name uslab.
	TeamNotFound TeamProblem = "not_found"
	// TeamMultiple: more than one Team has the key or name uslab.
	TeamMultiple TeamProblem = "multiple"
)

// Message is the text the Dashboard and GET /api/linear/status show (spec
// 15 3.2, 6章, Q216).
func (p TeamProblem) Message() string {
	switch p {
	case TeamNotFound:
		return "照会先 Team（uslab）が見つからない"
	case TeamMultiple:
		return "照会先 Team（uslab）が複数ある"
	}
	return ""
}

// TeamUnresolvedError is returned by a TeamSource when the Team list was read
// but no single polling Team matched. The Poller does not query (spec 15 6章).
type TeamUnresolvedError struct{ Problem TeamProblem }

func (e *TeamUnresolvedError) Error() string {
	return "linearintake: polling team unresolved: " + string(e.Problem)
}

// TeamListError is returned by a TeamSource when the Team list could not be
// read from Linear. The Poller records it as a failed query.
type TeamListError struct{ Err error }

func (e *TeamListError) Error() string { return "linearintake: list teams: " + e.Err.Error() }
func (e *TeamListError) Unwrap() error { return e.Err }

// TeamLister reads Linear's Team list. *linear.Client implements it.
type TeamLister interface {
	ListTeams(ctx context.Context) ([]linear.Team, error)
}

// SelectPollTeam picks the one Team whose key or name is uslab, ignoring
// case (spec 15 3.2, Q216). It returns the problem when none or several
// match. A Team whose key and name both match counts once.
func SelectPollTeam(teams []linear.Team) (string, TeamProblem) {
	matched := map[string]bool{}
	var id string
	for _, team := range teams {
		if strings.EqualFold(team.Key, PollTeamName) || strings.EqualFold(team.Name, PollTeamName) {
			matched[team.ID] = true
			id = team.ID
		}
	}
	switch len(matched) {
	case 0:
		return "", TeamNotFound
	case 1:
		return id, ""
	default:
		return "", TeamMultiple
	}
}

// UslabTeams is the production TeamSource (spec 15 3.2, Q216). It reads the
// Team list once, at the first round (evaloomd's startup), and keeps the
// Team ID while queries succeed. After a failed query the Poller calls
// InvalidatePollTeam, so the list is read again before the next query. When
// no single Team matched, nothing is kept and the next round reads again.
type UslabTeams struct {
	Lister TeamLister

	mu sync.Mutex
	id string // "" until resolved, and after InvalidatePollTeam
}

// NewUslabTeams resolves the polling Team with lister.
func NewUslabTeams(lister TeamLister) *UslabTeams { return &UslabTeams{Lister: lister} }

// LinearPollTeamID returns the kept Team ID, reading the Team list first
// when none is kept.
func (u *UslabTeams) LinearPollTeamID(ctx context.Context) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.id != "" {
		return u.id, nil
	}
	teams, err := u.Lister.ListTeams(ctx)
	if err != nil {
		return "", &TeamListError{Err: err}
	}
	id, problem := SelectPollTeam(teams)
	if problem != "" {
		return "", &TeamUnresolvedError{Problem: problem}
	}
	u.id = id
	return id, nil
}

// InvalidatePollTeam forgets the Team ID so the next round reads the Team
// list again (spec 15 3.2: 照会がエラーになったら引き直す).
func (u *UslabTeams) InvalidatePollTeam() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.id = ""
}

// teamInvalidator is implemented by a TeamSource that keeps the Team ID.
type teamInvalidator interface{ InvalidatePollTeam() }

// PollTeamStatus holds the latest reason the Poller could not choose a
// polling Team, for the Dashboard and GET /api/linear/status (spec 15 3.2,
// 5章, 6章). It lives in the process: the Poller sets it each round.
type PollTeamStatus struct {
	mu      sync.Mutex
	problem TeamProblem
}

// PollTeamProblem returns the current problem, or "" when the last round
// resolved the polling Team (or no round has decided yet).
func (s *PollTeamStatus) PollTeamProblem() TeamProblem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.problem
}

func (s *PollTeamStatus) set(problem TeamProblem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.problem = problem
}
