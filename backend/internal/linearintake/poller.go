// Package linearintake runs the Control Plane's Linear Poller (spec 15 3.1,
// 3.2 and 6章). It queries every registered Team once at startup and then at
// each interval, one round at a time, and records per-Team success or failure
// in linear_poll_teams. What to do with the Issues read is delegated to an
// IssueHandler; Intake (intake.go) decides attach and detach (spec 15 3.4).
package linearintake

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// DefaultInterval is the polling interval (spec 15 3.1 and 4章, Q43).
const DefaultInterval = time.Minute

// DefaultTriggerLabel is the default of the global setting
// linear.triggerLabel (spec 15 3.4, Q48).
const DefaultTriggerLabel = "ai-ready"

// FailureAlertThreshold is the consecutive failure count at which the owner
// is alerted when the failure at that count is not auth (spec 15 6章,
// decided 2026-11-08, Q86).
const FailureAlertThreshold = 5

// Client is the subset of *linear.Client the Poller uses.
type Client interface {
	CheckKey() error
	ListReadyIssues(ctx context.Context, teamID, label string) ([]linear.Issue, error)
}

// IssueHandler receives one Team's complete list after it has been read to
// the last page. It is never called with a partial list (spec 15 3.2).
// Intake implements it.
type IssueHandler interface {
	HandleTeamIssues(ctx context.Context, team store.LinearPollTeam, issues []linear.Issue) error
}

// Alert tells the owner that polling keeps failing, or that it hit an
// authentication error (spec 12 3.5). It carries only the classification
// and count, never the API key, a response body or a Team ID.
type Alert struct {
	Class               linear.ErrorClass
	ConsecutiveFailures int
}

// TeamSource names the Team to poll. An empty ID means no polling Team: the
// round queries nothing (spec 15 3.2, 6章). A *TeamUnresolvedError means the
// uslab Team is missing or ambiguous: the round queries nothing and the
// problem is published in PollTeamStatus. A *TeamListError means the Team
// list could not be read: the round is recorded as a failed query. UslabTeams
// is the production implementation (Q216).
type TeamSource interface {
	LinearPollTeamID(ctx context.Context) (string, error)
}

// noTeams is the TeamSource of a Poller whose Client cannot list Teams.
type noTeams struct{}

func (noTeams) LinearPollTeamID(context.Context) (string, error) { return "", nil }

// Alerter delivers an Alert to the owner (macOS notification, spec 15 6章).
type Alerter interface {
	AlertLinearPollFailing(ctx context.Context, alert Alert) error
}

// Poller polls Linear. Fields must be set before Run or PollOnce.
type Poller struct {
	DB           *sql.DB
	Client       Client
	Handler      IssueHandler // nil: lists are read and discarded
	Alerter      Alerter      // nil: alerts are only logged
	Logger       *slog.Logger
	Interval     time.Duration // zero: DefaultInterval
	TriggerLabel string        // empty: Settings, then DefaultTriggerLabel
	// Settings supplies the global label settings, read once per round and
	// passed to Handler through the context. nil: TriggerLabel and defaults.
	Settings LabelSource
	// Teams names the polling Team each round. nil: UslabTeams over Client
	// when Client lists Teams, otherwise no polling Team.
	Teams TeamSource
	// TeamStatus receives why no polling Team could be chosen, for the
	// Dashboard and GET /api/linear/status (Q216). nil: the Poller's own.
	TeamStatus *PollTeamStatus
	Now        func() time.Time
	// After waits between rounds, like time.After. nil: a real timer. Tests
	// set it to drive Run without sleeping.
	After func(time.Duration) <-chan time.Time
	// AuthGate is the authentication error notification suppression shared
	// with the Linear comment Worker (spec 16 3.4.3, Q84). nil: the Poller
	// uses a gate of its own.
	AuthGate *linear.AuthAlertGate

	mu sync.Mutex // one round at a time (spec 15 3.1)
	// alerted holds Teams already alerted in their current failure streak, so
	// one streak produces one "5 failures" notification. It is cleared by a
	// success. Authentication errors never use it (Q86).
	alerted   map[string]bool
	ownGate   *linear.AuthAlertGate
	ownTeams  TeamSource
	ownStatus *PollTeamStatus
}

// Result summarizes one round.
type Result struct {
	// Polled is false when there is no polling Team (TeamSource gave "").
	Polled bool
	// ResumeAt is Linear's restart hint after rate limiting; zero otherwise.
	ResumeAt time.Time
}

// Run polls immediately, then once per interval until ctx ends. A round that
// outlasts the interval delays the next one rather than overlapping it, and
// a rate-limit restart hint later than the next interval postpones polling.
func (p *Poller) Run(ctx context.Context) {
	for {
		started := p.now()
		result, err := p.PollOnce(ctx)
		if err != nil && ctx.Err() == nil {
			p.logger().Error("linear poll round failed", "error", err)
		}
		next := started.Add(p.interval())
		if result.ResumeAt.After(next) {
			next = result.ResumeAt
		}
		if !p.wait(ctx, max(next.Sub(p.now()), 0)) {
			return
		}
	}
}

// wait blocks for d and reports false when ctx ended first.
func (p *Poller) wait(ctx context.Context, d time.Duration) bool {
	if p.After != nil {
		select {
		case <-ctx.Done():
			return false
		case <-p.After(d):
			return true
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// PollOnce runs one round. Concurrent calls are serialized, so two rounds
// never run at the same time. The returned error is a database error; Linear
// failures are recorded per Team instead.
func (p *Poller) PollOnce(ctx context.Context) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var result Result
	var alerts []Alert
	var errs error
	record := func(team store.LinearPollTeam, class linear.ErrorClass) {
		alert, err := p.recordFailure(ctx, team.TeamID, class)
		errs = errors.Join(errs, err)
		if alert != nil {
			alerts = append(alerts, *alert)
		}
	}
	defer func() { p.alert(ctx, alerts) }()

	source := p.teamSource()
	// After a failed query the Team ID is read again before the next one
	// (spec 15 3.2, Q216).
	invalidate := func() {
		if inv, ok := source.(teamInvalidator); ok {
			inv.InvalidatePollTeam()
		}
	}
	teamID, err := source.LinearPollTeamID(ctx)
	var unresolved *TeamUnresolvedError
	var listErr *TeamListError
	switch {
	case errors.As(err, &unresolved):
		// No single uslab Team: stop intake without querying, and leave
		// linear_poll_teams as it is (spec 15 6章).
		p.teamStatus().set(unresolved.Problem)
		p.logger().Warn("linear poll skipped: polling team unresolved", "team", PollTeamName, "problem", string(unresolved.Problem))
		return result, nil
	case errors.As(err, &listErr):
		if ctx.Err() != nil {
			return result, nil
		}
		// The Team list is a query too. Its failure is recorded on the
		// polling Team rows already known, like a failed issue list.
		class := linear.ClassOf(listErr.Err)
		p.logger().Warn("linear team list failed", "class", string(class))
		known, err := store.ListLinearPollTeams(ctx, p.DB)
		if err != nil {
			return result, err
		}
		for _, team := range known {
			record(team, class)
		}
		if len(known) == 0 && class == linear.ClassAuth && p.gate().Acquire() {
			// No Team row to record on yet (first start), but an
			// authentication error still alerts once through the shared
			// suppression (spec 15 6章, Q84).
			alerts = append(alerts, Alert{Class: class, ConsecutiveFailures: 1})
		}
		var linearErr *linear.Error
		if class == linear.ClassRateLimited && errors.As(listErr.Err, &linearErr) {
			result.ResumeAt = linearErr.ResetAt
		}
		return result, errs
	case err != nil:
		return result, err
	}
	p.teamStatus().set("")
	teams, err := store.SyncLinearPollTeam(ctx, p.DB, teamID)
	if err != nil || len(teams) == 0 {
		// No registered Team: do not query (spec 15 3.2), not even the key.
		return result, err
	}
	result.Polled = true

	// An unusable key file stops the round before any query; every Team is
	// marked auth (spec 15 6章). CheckKey logs the reason without the value.
	if err := p.Client.CheckKey(); err != nil {
		invalidate()
		for _, team := range teams {
			record(team, linear.ClassAuth)
		}
		return result, errs
	}
	if p.Settings != nil {
		ctx = withRoundLabels(ctx, p.Settings.LinearLabels(ctx))
	}
	label := p.TriggerLabel
	if label == "" {
		label = roundLabels(ctx).TriggerLabel
	}
	for _, team := range teams {
		issues, err := p.Client.ListReadyIssues(ctx, team.TeamID, label)
		if ctx.Err() != nil {
			// Shutdown is not a Linear failure; leave the row unchanged.
			return result, errs
		}
		if err == nil {
			// A Team's query succeeded, so the key is valid again: lift the
			// shared suppression (spec 16 3.4.3 step 4).
			p.gate().Release()
		}
		if err != nil {
			class := linear.ClassOf(err)
			p.logger().Warn("linear poll failed", "team", team.TeamID, "class", string(class))
			invalidate()
			record(team, class)
			var linearErr *linear.Error
			if class == linear.ClassRateLimited {
				// The limit applies to the API key, so stop the whole round
				// until Linear's restart hint (spec 15 6章).
				if errors.As(err, &linearErr) {
					result.ResumeAt = linearErr.ResetAt
				}
				return result, errs
			}
			continue
		}
		if err := store.RecordLinearPollSuccess(ctx, p.DB, team.TeamID, p.now()); err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		delete(p.alerted, team.TeamID)
		if p.Handler != nil {
			if err := p.Handler.HandleTeamIssues(ctx, team, issues); err != nil {
				p.logger().Error("linear intake failed", "team", team.TeamID, "error", err)
			}
		}
	}
	return result, errs
}

// teamSource returns Teams, or the Poller's own UslabTeams over Client.
// It is called with p.mu held.
func (p *Poller) teamSource() TeamSource {
	if p.Teams != nil {
		return p.Teams
	}
	if p.ownTeams == nil {
		if lister, ok := p.Client.(TeamLister); ok {
			p.ownTeams = NewUslabTeams(lister)
		} else {
			p.ownTeams = noTeams{}
		}
	}
	return p.ownTeams
}

// teamStatus returns TeamStatus, or the Poller's own. It is called with
// p.mu held.
func (p *Poller) teamStatus() *PollTeamStatus {
	if p.TeamStatus != nil {
		return p.TeamStatus
	}
	if p.ownStatus == nil {
		p.ownStatus = &PollTeamStatus{}
	}
	return p.ownStatus
}

// recordFailure stores the failure and decides whether it warrants an alert
// (spec 15 6章, spec 12 3.5, Q84, Q86):
//   - An authentication error (including an unusable key file) alerts only
//     through the suppression shared with the comment Worker, never through
//     the Team's streak.
//   - Any other failure alerts when the Team's streak reaches the threshold,
//     once per streak.
func (p *Poller) recordFailure(ctx context.Context, teamID string, class linear.ErrorClass) (*Alert, error) {
	before, after, ok, err := store.RecordLinearPollFailure(ctx, p.DB, teamID, string(class))
	if err != nil || !ok {
		return nil, err
	}
	if class == linear.ClassAuth {
		if !p.gate().Acquire() {
			return nil, nil
		}
		return &Alert{Class: class, ConsecutiveFailures: after.ConsecutiveFailures}, nil
	}
	reached := before.ConsecutiveFailures < FailureAlertThreshold && after.ConsecutiveFailures >= FailureAlertThreshold
	if !reached || p.alerted[teamID] {
		return nil, nil
	}
	if p.alerted == nil {
		p.alerted = map[string]bool{}
	}
	p.alerted[teamID] = true
	return &Alert{Class: class, ConsecutiveFailures: after.ConsecutiveFailures}, nil
}

// gate returns the shared suppression, or the Poller's own when none is set.
// It is called with p.mu held.
func (p *Poller) gate() *linear.AuthAlertGate {
	if p.AuthGate != nil {
		return p.AuthGate
	}
	if p.ownGate == nil {
		p.ownGate = &linear.AuthAlertGate{}
	}
	return p.ownGate
}

// alert sends at most one notification per round, however many Teams failed.
// An authentication alert wins: the shared suppression is already held for
// it, so dropping it would leave the owner without the alert (Q84).
func (p *Poller) alert(ctx context.Context, alerts []Alert) {
	if len(alerts) == 0 {
		return
	}
	alert := alerts[0]
	for _, a := range alerts[1:] {
		aAuth, alertAuth := a.Class == linear.ClassAuth, alert.Class == linear.ClassAuth
		if (aAuth && !alertAuth) || (aAuth == alertAuth && a.ConsecutiveFailures > alert.ConsecutiveFailures) {
			alert = a
		}
	}
	p.logger().Warn("linear polling keeps failing", "class", string(alert.Class), "consecutive_failures", alert.ConsecutiveFailures)
	if p.Alerter == nil {
		return
	}
	if err := p.Alerter.AlertLinearPollFailing(ctx, alert); err != nil {
		p.logger().Error("linear poll alert failed", "error", err)
	}
}

func (p *Poller) interval() time.Duration {
	if p.Interval <= 0 {
		return DefaultInterval
	}
	return p.Interval
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Poller) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}
