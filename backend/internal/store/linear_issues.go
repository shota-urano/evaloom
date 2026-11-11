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
	// RemovedAt is the removed mark (spec 15 3.8, Q78): when the Issue was
	// found deleted or archived. Empty: NULL, the Issue is not marked.
	RemovedAt string
	// RecheckRequestedAt is the recheck mark (spec 15 3.6.3, 3.8, Q81): when
	// an Evaloom setting change asked to redo the held task's decision.
	// Empty: NULL, no recheck requested.
	RecheckRequestedAt string
}

const linearIssueStateColumns = `linear_issue_id, issue_identifier, team_id, ai_ready, attach_seq,
	issue_updated_at, coalesce(last_decision, ''), coalesce(last_decision_at, ''), first_seen_at, last_seen_at,
	coalesce(removed_at, ''), coalesce(recheck_requested_at, '')`

func scanLinearIssueState(row interface{ Scan(...any) error }) (LinearIssueState, error) {
	var s LinearIssueState
	err := row.Scan(&s.LinearIssueID, &s.IssueIdentifier, &s.TeamID, &s.AIReady, &s.AttachSeq,
		&s.IssueUpdatedAt, &s.LastDecision, &s.LastDecisionAt, &s.FirstSeenAt, &s.LastSeenAt, &s.RemovedAt,
		&s.RecheckRequestedAt)
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
// trigger label in the previous poll (ai_ready=1), ordered by Issue ID. An
// empty teamID returns the rows of every Team (owner status view, spec 15
// 5章), ordered by Team and Issue ID.
func ListAttachedLinearIssues(ctx context.Context, db *sql.DB, teamID string) ([]LinearIssueState, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+linearIssueStateColumns+`
		FROM linear_issue_states WHERE (? = '' OR team_id = ?) AND ai_ready = 1
		ORDER BY team_id, linear_issue_id`, teamID, teamID)
	if err != nil {
		return nil, fmt.Errorf("store: list attached linear issues: %w", err)
	}
	states := []LinearIssueState{}
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
// from an existing row. An empty RemovedAt clears the removed mark and an
// empty RecheckRequestedAt clears the recheck mark.
func SaveLinearIssueState(ctx context.Context, tx *sql.Tx, s LinearIssueState) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO linear_issue_states (linear_issue_id, issue_identifier,
		team_id, ai_ready, attach_seq, issue_updated_at, last_decision, last_decision_at, first_seen_at, last_seen_at,
		removed_at, recheck_requested_at)
		VALUES (?, ?, ?, ?, ?, ?, nullif(?, ''), nullif(?, ''), ?, ?, nullif(?, ''), nullif(?, ''))
		ON CONFLICT (linear_issue_id) DO UPDATE SET issue_identifier = excluded.issue_identifier,
			team_id = excluded.team_id, ai_ready = excluded.ai_ready, attach_seq = excluded.attach_seq,
			issue_updated_at = excluded.issue_updated_at, last_decision = excluded.last_decision,
			last_decision_at = excluded.last_decision_at, last_seen_at = excluded.last_seen_at,
			removed_at = excluded.removed_at, recheck_requested_at = excluded.recheck_requested_at`,
		s.LinearIssueID, s.IssueIdentifier, s.TeamID, s.AIReady, s.AttachSeq, s.IssueUpdatedAt,
		s.LastDecision, s.LastDecisionAt, s.FirstSeenAt, s.LastSeenAt, s.RemovedAt, s.RecheckRequestedAt)
	if err != nil {
		return fmt.Errorf("store: save linear issue state: %w", err)
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

// LinearIssueLink is one row of linear_issue_links (spec 15 3.8).
type LinearIssueLink struct {
	ID              string
	LinearIssueID   string
	IssueIdentifier string
	IssueURL        string
	TeamID          string
	TaskID          string
	AttachSeq       int
	CreatedAt       string
}

// InsertLinearIssueLink records which task an attach started. The schema
// rejects a second task for the same attach (UNIQUE(linear_issue_id,
// attach_seq)) and a second link for one task.
func InsertLinearIssueLink(ctx context.Context, tx *sql.Tx, l LinearIssueLink) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier,
		issue_url, team_id, task_id, attach_seq, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.LinearIssueID, l.IssueIdentifier, l.IssueURL, l.TeamID, l.TaskID, l.AttachSeq, l.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: insert linear issue link: %w", err)
	}
	return nil
}

// LinearRoute is the Project destination consumed by the existing intake
// boundary. Special-harness intake is implemented separately from this task.
type LinearRoute struct{ ProjectID string }

// ListLinearRoutesByLabel resolves the globally unique label without Team scope.
func ListLinearRoutesByLabel(ctx context.Context, tx *sql.Tx, label string) ([]LinearRoute, error) {
	rows, err := tx.QueryContext(ctx, `SELECT project_id FROM routes WHERE label = ? AND project_id IS NOT NULL`, label)
	if err != nil {
		return nil, fmt.Errorf("store: list linear routes: %w", err)
	}
	var routes []LinearRoute
	for rows.Next() {
		var r LinearRoute
		if err := rows.Scan(&r.ProjectID); err != nil {
			rows.Close()
			return nil, err
		}
		routes = append(routes, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return routes, nil
}

// HeldLinearTask is a task held at intake (spec 15 3.6.3): source=linear,
// blocked with an intake_block_cause and without an Execution.
type HeldLinearTask struct {
	TaskID string
	Cause  string
}

// ListHeldLinearTasks returns the held tasks linked to the Issue through
// linear_issue_links, oldest attach first.
func ListHeldLinearTasks(ctx context.Context, tx *sql.Tx, issueID string) ([]HeldLinearTask, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.id, coalesce(t.intake_block_cause, 'routing') FROM linear_issue_links l
		JOIN tasks t ON t.id = l.task_id
		WHERE l.linear_issue_id = ? AND t.source = 'linear'
		AND (t.status = 'routing' OR (t.status = 'blocked' AND t.intake_block_cause IS NOT NULL))
		ORDER BY l.attach_seq`, issueID)
	if err != nil {
		return nil, fmt.Errorf("store: list held linear tasks: %w", err)
	}
	var held []HeldLinearTask
	for rows.Next() {
		var h HeldLinearTask
		if err := rows.Scan(&h.TaskID, &h.Cause); err != nil {
			rows.Close()
			return nil, err
		}
		held = append(held, h)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return held, nil
}

// LinearRecheck selects the held tasks an Evaloom setting change may release
// (spec 15 3.6.3, the table of setting changes, Q81).
type LinearRecheck struct {
	// ProjectID is the changed Project: its config_missing_assignment tasks
	// are selected.
	ProjectID string
	// Routing is true when the Project's linear_team_id or
	// linear_routing_label changed: every routing_unknown_label and
	// routing_team_mismatch task is selected as well.
	Routing bool
}

// MarkLinearRecheck puts the recheck mark (recheck_requested_at) on the
// linear_issue_states rows of the Issues whose held task the change selects.
// It runs in the transaction that writes the setting, so a rolled-back write
// leaves no mark. An existing mark keeps its earlier time. Tasks held for
// routing_multiple_labels, routing_no_label or missing_acceptance_criteria
// depend only on the Issue and are never marked.
func MarkLinearRecheck(ctx context.Context, tx *sql.Tx, change LinearRecheck, at string) error {
	_, err := tx.ExecContext(ctx, `UPDATE linear_issue_states
		SET recheck_requested_at = coalesce(recheck_requested_at, ?)
		WHERE linear_issue_id IN (SELECT l.linear_issue_id FROM linear_issue_links l
			JOIN tasks t ON t.id = l.task_id
			WHERE t.source = 'linear' AND (
				(t.status = 'blocked' AND t.intake_block_cause = 'config_missing_profile' AND t.project_id = ?)
				OR (? AND t.status = 'routing')))`,
		at, change.ProjectID, change.Routing)
	if err != nil {
		return fmt.Errorf("store: mark linear recheck: %w", err)
	}
	return nil
}

// UpdateHeldLinearTask writes the redone 3.6 decision to a held task (spec 15
// 3.6.3): projectID (empty: NULL), acceptance criteria and cause (empty:
// NULL, which releases the hold). Only a held task without an Execution is
// changed; anything else is an error. A routing reason changes status to routing;
// clearing the cause preserves status until the initial Execution is queued.
func UpdateHeldLinearTask(ctx context.Context, tx *sql.Tx, taskID, projectID, criteria, cause, updatedAt string) error {
	status := ""
	if cause != "" {
		status, cause = intakeTaskState("blocked", cause)
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET project_id = nullif(?, ''), acceptance_criteria = ?,
		intake_block_cause = nullif(?, ''), updated_at = ?, status=CASE WHEN ?='' THEN status ELSE ? END
		WHERE id = ? AND source = 'linear'
		AND (status='routing' OR (status='blocked' AND intake_block_cause IS NOT NULL))
		AND NOT EXISTS (SELECT 1 FROM executions e WHERE e.task_id = tasks.id)`,
		projectID, criteria, cause, updatedAt, status, status, taskID)
	if err != nil {
		return fmt.Errorf("store: update held linear task: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("store: held linear task %s changed before the recheck", taskID)
	}
	return nil
}

// ListCancellableLinearTasks returns the IDs of the tasks linked to the Issue
// through linear_issue_links that a Linear-caused cancellation stops (spec 15
// 3.7): queued, running, verifying or blocked (held included), oldest attach
// first. Terminal tasks are never returned.
func ListCancellableLinearTasks(ctx context.Context, tx *sql.Tx, issueID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.id FROM linear_issue_links l
		JOIN tasks t ON t.id = l.task_id
		WHERE l.linear_issue_id = ? AND t.status IN ('routing', 'queued', 'running', 'blocked')
		ORDER BY l.attach_seq`, issueID)
	if err != nil {
		return nil, fmt.Errorf("store: list cancellable linear tasks: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return ids, nil
}
