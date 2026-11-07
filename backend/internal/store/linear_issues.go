package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LinearIssueState is one row of linear_issue_states (spec 15 3.3, 3.8): how
// an Issue looked in the previous poll. Whether a task was started or has
// finished is read from linear_issue_links and tasks, not from this row.
type LinearIssueState struct {
	LinearIssueID   string
	IssueIdentifier string
	TeamID          string
	AIReady         bool
	AttachSeq       int
	IssueUpdatedAt  string
	LastDecision    string // empty: NULL
	LastDecisionAt  string // empty: NULL
	FirstSeenAt     string
	LastSeenAt      string
}

const linearIssueStateColumns = `linear_issue_id, issue_identifier, team_id, ai_ready, attach_seq,
	issue_updated_at, coalesce(last_decision, ''), coalesce(last_decision_at, ''), first_seen_at, last_seen_at`

func scanLinearIssueState(row interface{ Scan(...any) error }) (LinearIssueState, error) {
	var s LinearIssueState
	err := row.Scan(&s.LinearIssueID, &s.IssueIdentifier, &s.TeamID, &s.AIReady, &s.AttachSeq,
		&s.IssueUpdatedAt, &s.LastDecision, &s.LastDecisionAt, &s.FirstSeenAt, &s.LastSeenAt)
	return s, err
}

// GetLinearIssueState reads one Issue's previous-poll record. ok is false
// when the Issue has no row.
func GetLinearIssueState(ctx context.Context, tx *sql.Tx, issueID string) (state LinearIssueState, ok bool, err error) {
	state, err = scanLinearIssueState(tx.QueryRowContext(ctx, `SELECT `+linearIssueStateColumns+`
		FROM linear_issue_states WHERE linear_issue_id = ?`, issueID))
	if errors.Is(err, sql.ErrNoRows) {
		return LinearIssueState{}, false, nil
	}
	if err != nil {
		return LinearIssueState{}, false, fmt.Errorf("store: read linear issue state: %w", err)
	}
	return state, true, nil
}

// ListAttachedLinearIssues returns the rows of teamID that carried the
// trigger label in the previous poll (ai_ready=1), ordered by Issue ID.
func ListAttachedLinearIssues(ctx context.Context, db *sql.DB, teamID string) ([]LinearIssueState, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+linearIssueStateColumns+`
		FROM linear_issue_states WHERE team_id = ? AND ai_ready = 1 ORDER BY linear_issue_id`, teamID)
	if err != nil {
		return nil, fmt.Errorf("store: list attached linear issues: %w", err)
	}
	var states []LinearIssueState
	for rows.Next() {
		state, err := scanLinearIssueState(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		states = append(states, state)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return states, nil
}

// SaveLinearIssueState inserts or replaces the row. first_seen_at is kept
// from an existing row.
func SaveLinearIssueState(ctx context.Context, tx *sql.Tx, s LinearIssueState) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO linear_issue_states (linear_issue_id, issue_identifier,
		team_id, ai_ready, attach_seq, issue_updated_at, last_decision, last_decision_at, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, nullif(?, ''), nullif(?, ''), ?, ?)
		ON CONFLICT (linear_issue_id) DO UPDATE SET issue_identifier = excluded.issue_identifier,
			team_id = excluded.team_id, ai_ready = excluded.ai_ready, attach_seq = excluded.attach_seq,
			issue_updated_at = excluded.issue_updated_at, last_decision = excluded.last_decision,
			last_decision_at = excluded.last_decision_at, last_seen_at = excluded.last_seen_at`,
		s.LinearIssueID, s.IssueIdentifier, s.TeamID, s.AIReady, s.AttachSeq, s.IssueUpdatedAt,
		s.LastDecision, s.LastDecisionAt, s.FirstSeenAt, s.LastSeenAt)
	if err != nil {
		return fmt.Errorf("store: save linear issue state: %w", err)
	}
	return nil
}

// DeleteLinearIssueState removes the row of an Issue that was deleted or
// archived in Linear (spec 15 3.4).
func DeleteLinearIssueState(ctx context.Context, tx *sql.Tx, issueID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM linear_issue_states WHERE linear_issue_id = ?`, issueID); err != nil {
		return fmt.Errorf("store: delete linear issue state: %w", err)
	}
	return nil
}

// HasActiveLinearTask reports whether a task linked to the Issue through
// linear_issue_links is not in a terminal state (spec 15 3.5 rule 3, Q46).
func HasActiveLinearTask(ctx context.Context, tx *sql.Tx, issueID string) (bool, error) {
	var active bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM linear_issue_links l
		JOIN tasks t ON t.id = l.task_id
		WHERE l.linear_issue_id = ? AND t.status NOT IN ('completed', 'failed', 'cancelled', 'timed_out'))`,
		issueID).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("store: check active linear task: %w", err)
	}
	return active, nil
}

// IsLinearTeamPolled reports whether teamID is one of the Teams polled this
// round (a row of linear_poll_teams, kept in step with the registered
// Projects by SyncLinearPollTeams).
func IsLinearTeamPolled(ctx context.Context, tx *sql.Tx, teamID string) (bool, error) {
	var polled bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM linear_poll_teams WHERE team_id = ?)`,
		teamID).Scan(&polled); err != nil {
		return false, fmt.Errorf("store: check linear poll team: %w", err)
	}
	return polled, nil
}

// MarkLinearBaselineDone records that the Team's first poll (spec 15 3.4,
// Q64) finished. An existing time is kept.
func MarkLinearBaselineDone(ctx context.Context, db *sql.DB, teamID string, at time.Time) error {
	_, err := db.ExecContext(ctx, `UPDATE linear_poll_teams SET baseline_done_at = ?
		WHERE team_id = ? AND baseline_done_at IS NULL`, at.UTC().Format(time.RFC3339Nano), teamID)
	if err != nil {
		return fmt.Errorf("store: mark linear baseline done: %w", err)
	}
	return nil
}
