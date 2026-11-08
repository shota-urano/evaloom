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

// UpdateLinearLabels applies patch and returns the resulting values. Each key
// whose value changes gets its settings row and one settings.update audit row
// with the values before and after, all in one transaction (spec 15 5章,
// spec 02 3.4, Q76). A value equal to the current one writes nothing.
func (s *LabelSettings) UpdateLinearLabels(ctx context.Context, patch LabelsPatch) (_ Labels, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Labels{}, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	rows, err := store.ReadSettings(ctx, tx)
	if err != nil {
		return Labels{}, err
	}
	labels := s.resolve(rows)
	for _, change := range []struct {
		key     string
		target  *string
		updated *string
	}{
		{SettingTriggerLabel, &labels.TriggerLabel, patch.TriggerLabel},
		{SettingRoutingLabelPrefix, &labels.RoutingLabelPrefix, patch.RoutingLabelPrefix},
	} {
		if change.updated == nil || *change.updated == *change.target {
			continue
		}
		encoded, err := json.Marshal(*change.updated)
		if err != nil {
			return Labels{}, err
		}
		if err := store.PutSetting(ctx, tx, change.key, string(encoded)); err != nil {
			return Labels{}, err
		}
		if _, err := store.RecordAuditLog(ctx, tx, store.AuditLog{Actor: "owner", Action: "settings.update",
			TargetType: "settings", TargetID: change.key,
			Detail: map[string]any{"before": *change.target, "after": *change.updated}}); err != nil {
			return Labels{}, fmt.Errorf("linearintake: audit setting %s: %w", change.key, err)
		}
		*change.target = *change.updated
	}
	if err := tx.Commit(); err != nil {
		return Labels{}, err
	}
	return labels, nil
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
