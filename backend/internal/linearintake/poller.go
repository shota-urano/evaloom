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
	Now      func() time.Time
	// AuthGate is the authentication error notification suppression shared
	// with the Linear comment Worker (spec 16 3.4.3, Q84). nil: the Poller
	// uses a gate of its own.
	AuthGate *linear.AuthAlertGate

	mu sync.Mutex // one round at a time (spec 15 3.1)
	// alerted holds Teams already alerted in their current failure streak, so
	// one streak produces one "5 failures" notification. It is cleared by a
	// success. Authentication errors never use it (Q86).
	alerted map[string]bool
	ownGate *linear.AuthAlertGate
}

// Result summarizes one round.
type Result struct {
	// Polled is false when no Project registers a linear_team_id.
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
		timer := time.NewTimer(max(next.Sub(p.now()), 0))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// PollOnce runs one round. Concurrent calls are serialized, so two rounds
// never run at the same time. The returned error is a database error; Linear
// failures are recorded per Team instead.
func (p *Poller) PollOnce(ctx context.Context) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var result Result
	teams, err := store.SyncLinearPollTeams(ctx, p.DB)
	if err != nil || len(teams) == 0 {
		// No registered Team: do not query (spec 15 3.2), not even the key.
		return result, err
	}
	result.Polled = true
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

	// An unusable key file stops the round before any query; every Team is
	// marked auth (spec 15 6章). CheckKey logs the reason without the value.
	if err := p.Client.CheckKey(); err != nil {
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
