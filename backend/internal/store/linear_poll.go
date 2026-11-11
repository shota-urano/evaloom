package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LinearPollTeam is one row of linear_poll_teams (spec 15 3.8). LastError is
// only a classification (auth / rate_limited / network / other), never a
// response body or the API key.
type LinearPollTeam struct {
	TeamID              string
	BaselineDone        bool
	LastSuccessAt       string
	LastError           string
	ConsecutiveFailures int
}

// Polling scope comes from the global setting; routes have no Team column.
const registeredLinearTeams = `SELECT json_extract(value, '$') AS team_id FROM settings
 WHERE key='linear.teamId' AND json_valid(value)
 AND json_type(CASE WHEN json_valid(value) THEN value ELSE 'null' END)='text'
 AND trim(json_extract(value, '$')) <> ''`

// PruneLinearPollTeams removes baselines outside settings.linear.teamId.
func PruneLinearPollTeams(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM linear_poll_teams WHERE team_id NOT IN (`+registeredLinearTeams+`)`); err != nil {
		return fmt.Errorf("store: remove unregistered linear poll teams: %w", err)
	}
	return nil
}

// SyncLinearPollTeams returns the configured polling Team, creates its baseline
// if needed, and removes the previous Team's baseline in the same transaction.
func SyncLinearPollTeams(ctx context.Context, db *sql.DB) (_ []LinearPollTeam, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	if err = PruneLinearPollTeams(ctx, tx); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO linear_poll_teams (team_id, consecutive_failures)
		SELECT team_id, 0 FROM (`+registeredLinearTeams+`) WHERE true
		ON CONFLICT (team_id) DO NOTHING`); err != nil {
		return nil, fmt.Errorf("store: add linear poll teams: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT team_id, baseline_done_at IS NOT NULL,
		coalesce(last_success_at, ''), coalesce(last_error, ''), consecutive_failures
		FROM linear_poll_teams ORDER BY team_id`)
	if err != nil {
		return nil, err
	}
	var teams []LinearPollTeam
	for rows.Next() {
		var team LinearPollTeam
		if err = rows.Scan(&team.TeamID, &team.BaselineDone, &team.LastSuccessAt, &team.LastError, &team.ConsecutiveFailures); err != nil {
			rows.Close()
			return nil, err
		}
		teams = append(teams, team)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return teams, nil
}

// RecordLinearPollSuccess marks a Team's list as read to the end: the error
// classification is cleared and the failure count returns to 0.
func RecordLinearPollSuccess(ctx context.Context, db *sql.DB, teamID string, at time.Time) error {
	_, err := db.ExecContext(ctx, `UPDATE linear_poll_teams
		SET last_success_at = ?, last_error = NULL, consecutive_failures = 0 WHERE team_id = ?`,
		at.UTC().Format(time.RFC3339Nano), teamID)
	if err != nil {
		return fmt.Errorf("store: record linear poll success: %w", err)
	}
	return nil
}

// RecordLinearPollFailure stores the failure classification and increments
// the consecutive failure count. It returns the row before and after the
// update so the caller can detect threshold crossings. ok is false when the
// Team has no row (it was unregistered meanwhile).
func RecordLinearPollFailure(ctx context.Context, db *sql.DB, teamID, class string) (before, after LinearPollTeam, ok bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return before, after, false, err
	}
	defer tx.Rollback()
	const query = `SELECT team_id, baseline_done_at IS NOT NULL, coalesce(last_success_at, ''),
		coalesce(last_error, ''), consecutive_failures FROM linear_poll_teams WHERE team_id = ?`
	scan := func(team *LinearPollTeam) error {
		return tx.QueryRowContext(ctx, query, teamID).Scan(&team.TeamID, &team.BaselineDone,
			&team.LastSuccessAt, &team.LastError, &team.ConsecutiveFailures)
	}
	if err = scan(&before); errors.Is(err, sql.ErrNoRows) {
		return before, after, false, nil
	} else if err != nil {
		return before, after, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE linear_poll_teams
		SET last_error = ?, consecutive_failures = consecutive_failures + 1 WHERE team_id = ?`, class, teamID); err != nil {
		return before, after, false, fmt.Errorf("store: record linear poll failure: %w", err)
	}
	if err = scan(&after); err != nil {
		return before, after, false, err
	}
	if err = tx.Commit(); err != nil {
		return before, after, false, err
	}
	return before, after, true, nil
}

// ListLinearPollTeams reads linear_poll_teams without changing it, for the
// owner's status view (spec 15 5章).
func ListLinearPollTeams(ctx context.Context, db *sql.DB) ([]LinearPollTeam, error) {
	rows, err := db.QueryContext(ctx, `SELECT team_id, baseline_done_at IS NOT NULL,
		coalesce(last_success_at, ''), coalesce(last_error, ''), consecutive_failures
		FROM linear_poll_teams ORDER BY team_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list linear poll teams: %w", err)
	}
	teams := []LinearPollTeam{}
	for rows.Next() {
		var team LinearPollTeam
		if err := rows.Scan(&team.TeamID, &team.BaselineDone, &team.LastSuccessAt, &team.LastError, &team.ConsecutiveFailures); err != nil {
			rows.Close()
			return nil, err
		}
		teams = append(teams, team)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return teams, nil
}
