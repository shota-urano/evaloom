package linearintake

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Labels are the global settings linear.triggerLabel and
// linear.routingLabelPrefix (spec 15 3.4 and 5章, Q48).
type Labels struct {
	TriggerLabel       string `json:"triggerLabel"`
	RoutingLabelPrefix string `json:"routingLabelPrefix"`
}

// Keys of the settings rows (spec 02 3.4, spec 15 3.4, Q76).
const (
	SettingTriggerLabel       = "linear.triggerLabel"
	SettingRoutingLabelPrefix = "linear.routingLabelPrefix"
)

// DefaultLabels are the defaults of the global settings: requirements 6.2's
// recommended ai-ready and repo: (spec 15 3.4, Q48).
func DefaultLabels() Labels {
	return Labels{TriggerLabel: DefaultTriggerLabel, RoutingLabelPrefix: DefaultRoutingLabelPrefix}
}

// LabelSource supplies the current global label settings. The Poller reads
// it once per round.
type LabelSource interface {
	LinearLabels(ctx context.Context) Labels
}

// LabelsPatch holds the keys a PATCH /api/settings supplies; nil keeps the
// current value. The caller has already rejected null and blank values.
type LabelsPatch struct {
	TriggerLabel       *string
	RoutingLabelPrefix *string
}

// LabelSettings stores the label settings in the settings table (spec 02 3.4,
// Q76), so they survive a Control Plane restart. The settings API and the
// Poller share it.
type LabelSettings struct {
	DB     *sql.DB
	Logger *slog.Logger // nil: slog.Default
}

// NewLabelSettings reads and writes the settings table of db.
func NewLabelSettings(db *sql.DB, logger *slog.Logger) *LabelSettings {
	return &LabelSettings{DB: db, Logger: logger}
}

func (s *LabelSettings) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

// LinearLabels returns the stored values. A key without a row uses its
// default; a row that is empty, blank or not a JSON string, or a table that
// cannot be read, also uses the default and is logged (spec 15 6章, Q76).
func (s *LabelSettings) LinearLabels(ctx context.Context) Labels {
	rows, err := store.ReadSettings(ctx, s.DB)
	if err != nil {
		s.logger().Error("settings could not be read; using the default linear labels", "error", err)
		return DefaultLabels()
	}
	return s.resolve(rows)
}

func (s *LabelSettings) resolve(rows map[string]string) Labels {
	labels := DefaultLabels()
	for key, target := range map[string]*string{
		SettingTriggerLabel:       &labels.TriggerLabel,
		SettingRoutingLabelPrefix: &labels.RoutingLabelPrefix,
	} {
		raw, ok := rows[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			s.logger().Warn("setting is not a string; using the default", "key", key, "default", *target)
			continue
		}
		if strings.TrimSpace(value) == "" {
			s.logger().Warn("setting is empty; using the default", "key", key, "default", *target)
			continue
		}
		*target = value
	}
	return labels
}

// Keys of the routing settings rows (spec 02 3.8, spec 15 3.6.4, Q111).
const (
	SettingRouteProposer   = "routing.proposer"
	SettingDefaultRunnerID = "routing.defaultRunnerId"
)

// RouteProposer is the global setting routing.proposer: the AI that
// proposes a route. A nil Model means no proposal is made (spec 02 3.8).
type RouteProposer struct {
	MicroHarness string  `json:"microHarness"`
	Model        *string `json:"model"`
}

func (p RouteProposer) equal(other RouteProposer) bool {
	if p.MicroHarness != other.MicroHarness || (p.Model == nil) != (other.Model == nil) {
		return false
	}
	return p.Model == nil || *p.Model == *other.Model
}

// DefaultRouteProposer is the default of routing.proposer (spec 02 3.8).
func DefaultRouteProposer() RouteProposer { return RouteProposer{MicroHarness: "claude_code"} }

// RoutingSettings are routing.proposer and routing.defaultRunnerId. A nil
// DefaultRunnerID means no Runner is set (the default, spec 02 3.8).
type RoutingSettings struct {
	Proposer        RouteProposer `json:"proposer"`
	DefaultRunnerID *string       `json:"defaultRunnerId"`
}

// Settings are the global settings served by GET/PATCH /api/settings (spec
// 15 5章): linear.triggerLabel, linear.routingLabelPrefix, routing.proposer
// and routing.defaultRunnerId. The polling Team is not a setting (Q216).
type Settings struct {
	Linear  Labels          `json:"linear"`
	Routing RoutingSettings `json:"routing"`
}

// SettingsPatch holds the keys a PATCH /api/settings supplies; nil keeps the
// current value. DefaultRunnerID points to nil to clear the setting (Q221).
// The caller has already rejected other null values and blank strings.
type SettingsPatch struct {
	TriggerLabel       *string
	RoutingLabelPrefix *string
	Proposer           *RouteProposer
	DefaultRunnerID    **string
}

// ErrTriggerLabelHasPrefix rejects settings in which linear.triggerLabel
// starts with linear.routingLabelPrefix (spec 15 Q160: 400). Field names
// the key the PATCH changed.
type ErrTriggerLabelHasPrefix struct{ Field string }

func (e *ErrTriggerLabelHasPrefix) Error() string {
	return "linearintake: trigger label starts with the routing label prefix"
}

// ErrRoutesOutsidePrefix rejects a routing label prefix that some routes
// label does not start with (spec 15 Q160: 422). Labels are the routes
// labels left outside the new prefix.
type ErrRoutesOutsidePrefix struct{ Labels []string }

func (e *ErrRoutesOutsidePrefix) Error() string {
	return "linearintake: routes labels do not start with the routing label prefix"
}

// Settings returns every global setting, each from its row or its default
// (spec 02 3.8, spec 15 6章).
func (s *LabelSettings) Settings(ctx context.Context) Settings {
	rows, err := store.ReadSettings(ctx, s.DB)
	if err != nil {
		s.logger().Error("settings could not be read; using the defaults", "error", err)
		return Settings{Linear: DefaultLabels(), Routing: RoutingSettings{Proposer: DefaultRouteProposer()}}
	}
	return Settings{Linear: s.resolve(rows), Routing: s.resolveRouting(rows)}
}

func (s *LabelSettings) resolveRouting(rows map[string]string) RoutingSettings {
	routing := RoutingSettings{Proposer: DefaultRouteProposer()}
	if raw, ok := rows[SettingRouteProposer]; ok {
		var proposer RouteProposer
		if err := json.Unmarshal([]byte(raw), &proposer); err != nil || strings.TrimSpace(proposer.MicroHarness) == "" {
			s.logger().Warn("setting is unusable; using the default", "key", SettingRouteProposer)
		} else {
			routing.Proposer = proposer
		}
	}
	if raw, ok := rows[SettingDefaultRunnerID]; ok {
		var id *string
		if err := json.Unmarshal([]byte(raw), &id); err != nil || (id != nil && strings.TrimSpace(*id) == "") {
			s.logger().Warn("setting is unusable; using the default", "key", SettingDefaultRunnerID)
		} else {
			routing.DefaultRunnerID = id
		}
	}
	return routing
}

// UpdateLinearLabels applies patch to the label settings and returns them.
func (s *LabelSettings) UpdateLinearLabels(ctx context.Context, patch LabelsPatch) (Labels, error) {
	settings, err := s.UpdateSettings(ctx, SettingsPatch{TriggerLabel: patch.TriggerLabel, RoutingLabelPrefix: patch.RoutingLabelPrefix})
	return settings.Linear, err
}

// UpdateSettings applies patch and returns the resulting settings. Each
// changed key updates or removes its settings row and gets one
// settings.update audit row with the values before and after, all in one
// transaction (spec 15 5章, spec 02 3.8, Q76, Q221). An unchanged value writes nothing.
// The label keys are checked against each other and the routes table as
// Q160 decides; a rejected patch writes nothing.
func (s *LabelSettings) UpdateSettings(ctx context.Context, patch SettingsPatch) (_ Settings, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Settings{}, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	rows, err := store.ReadSettings(ctx, tx)
	if err != nil {
		return Settings{}, err
	}
	current := Settings{Linear: s.resolve(rows), Routing: s.resolveRouting(rows)}
	next := current
	type change struct {
		key           string
		before, after any
	}
	var changes []change
	if patch.TriggerLabel != nil && *patch.TriggerLabel != current.Linear.TriggerLabel {
		next.Linear.TriggerLabel = *patch.TriggerLabel
		changes = append(changes, change{SettingTriggerLabel, current.Linear.TriggerLabel, next.Linear.TriggerLabel})
	}
	prefixChanged := patch.RoutingLabelPrefix != nil && *patch.RoutingLabelPrefix != current.Linear.RoutingLabelPrefix
	if prefixChanged {
		next.Linear.RoutingLabelPrefix = *patch.RoutingLabelPrefix
		changes = append(changes, change{SettingRoutingLabelPrefix, current.Linear.RoutingLabelPrefix, next.Linear.RoutingLabelPrefix})
	}
	if patch.Proposer != nil && !patch.Proposer.equal(current.Routing.Proposer) {
		next.Routing.Proposer = *patch.Proposer
		changes = append(changes, change{SettingRouteProposer, current.Routing.Proposer, next.Routing.Proposer})
	}
	if patch.DefaultRunnerID != nil {
		id, before := *patch.DefaultRunnerID, current.Routing.DefaultRunnerID
		if (id == nil) != (before == nil) || (id != nil && before != nil && *id != *before) {
			next.Routing.DefaultRunnerID = id
			changes = append(changes, change{SettingDefaultRunnerID, before, id})
		}
	}
	if len(changes) == 0 {
		return current, tx.Commit()
	}
	if err := checkLabels(ctx, tx, patch, next.Linear, prefixChanged); err != nil {
		return Settings{}, err
	}
	for _, c := range changes {
		if c.key == SettingDefaultRunnerID && next.Routing.DefaultRunnerID == nil {
			if err := store.DeleteSetting(ctx, tx, c.key); err != nil {
				return Settings{}, err
			}
		} else {
			encoded, err := json.Marshal(c.after)
			if err != nil {
				return Settings{}, err
			}
			if err := store.PutSetting(ctx, tx, c.key, string(encoded)); err != nil {
				return Settings{}, err
			}
		}
		if _, err := store.RecordAuditLog(ctx, tx, store.AuditLog{Actor: "owner", Action: "settings.update",
			TargetType: "settings", TargetID: c.key,
			Detail: map[string]any{"before": c.before, "after": c.after}}); err != nil {
			return Settings{}, fmt.Errorf("linearintake: audit setting %s: %w", c.key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Settings{}, err
	}
	return next, nil
}

// checkLabels applies Q160 to the labels a patch would leave: the trigger
// label must not start with the routing label prefix (400), and a changed
// prefix must still start every routes label (422).
func checkLabels(ctx context.Context, tx *sql.Tx, patch SettingsPatch, labels Labels, prefixChanged bool) error {
	if (patch.TriggerLabel != nil || prefixChanged) && strings.HasPrefix(labels.TriggerLabel, labels.RoutingLabelPrefix) {
		field := SettingRoutingLabelPrefix
		if patch.TriggerLabel != nil {
			field = SettingTriggerLabel
		}
		return &ErrTriggerLabelHasPrefix{Field: field}
	}
	if !prefixChanged {
		return nil
	}
	routes, err := store.RouteLabels(ctx, tx)
	if err != nil {
		return err
	}
	var outside []string
	for _, label := range routes {
		if !strings.HasPrefix(label, labels.RoutingLabelPrefix) {
			outside = append(outside, label)
		}
	}
	if len(outside) > 0 {
		return &ErrRoutesOutsidePrefix{Labels: outside}
	}
	return nil
}

type roundLabelsKey struct{}

// withRoundLabels carries the labels the Poller read at the start of a round
// to Intake and TaskStarter, so one round lists, detaches and routes with the
// same values even if the settings change meanwhile.
func withRoundLabels(ctx context.Context, labels Labels) context.Context {
	return context.WithValue(ctx, roundLabelsKey{}, labels)
}

// roundLabels returns the labels the Poller put in ctx, or the defaults.
// Empty values fall back to the defaults one by one.
func roundLabels(ctx context.Context) Labels {
	labels, _ := ctx.Value(roundLabelsKey{}).(Labels)
	if labels.TriggerLabel == "" {
		labels.TriggerLabel = DefaultTriggerLabel
	}
	if labels.RoutingLabelPrefix == "" {
		labels.RoutingLabelPrefix = DefaultRoutingLabelPrefix
	}
	return labels
}
