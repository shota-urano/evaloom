package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// WaitingApproval is one PR waiting for the owner's approval that the Runner
// reads periodically (spec 17 3.6, Q58). Target is approvals.target of the
// Execution's latest merge approval; Project is the snapshot the Git
// Publisher needs to authenticate to the repository.
type WaitingApproval struct {
	ExecutionID string          `json:"executionId"`
	TaskID      string          `json:"taskId"`
	Target      json.RawMessage `json:"target"`
	Project     ClaimProject    `json:"project"`
}

// WaitingApprovals lists the latest Executions in waiting_approval that this
// Runner ran, with the PR of their latest merge approval. Executions in
// merging are not listed: the approved operation (3.5.2) reads the PR itself
// and returns them to waiting_approval when the target changed.
func WaitingApprovals(ctx context.Context, db *sql.DB, runnerID string) ([]WaitingApproval, error) {
	rows, err := db.QueryContext(ctx, `SELECT e.id, e.task_id, t.project_id,
		(SELECT a.target FROM approvals a WHERE a.execution_id = e.id AND a.operation = 'merge_pull_request'
			ORDER BY a.created_at DESC, a.id DESC LIMIT 1)
		FROM executions e JOIN tasks t ON t.id = e.task_id JOIN projects p ON p.id = t.project_id
		WHERE e.status = 'waiting_approval' AND t.status = 'waiting_approval' AND e.runner_id = ? AND p.runner_id = ?
		AND e.seq = (SELECT max(seq) FROM executions WHERE task_id = e.task_id)
		ORDER BY e.id`, runnerID, runnerID)
	if err != nil {
		return nil, err
	}
	type row struct {
		item      WaitingApproval
		projectID string
	}
	var found []row
	for rows.Next() {
		var r row
		var target sql.NullString
		if err := rows.Scan(&r.item.ExecutionID, &r.item.TaskID, &r.projectID, &target); err != nil {
			rows.Close()
			return nil, err
		}
		if !target.Valid {
			continue // No PR approval to check.
		}
		r.item.Target = json.RawMessage(target.String)
		found = append(found, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	items := make([]WaitingApproval, 0, len(found))
	for _, r := range found {
		r.item.Project.Project, err = GetProject(ctx, db, r.projectID)
		if err != nil {
			return nil, err
		}
		items = append(items, r.item)
	}
	return items, nil
}

// HasMergeApprovalTx reports whether the Execution has a merge approval,
// i.e. a PR that was waiting for approval (spec 17 3.3).
func HasMergeApprovalTx(ctx context.Context, tx *sql.Tx, executionID string) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM approvals WHERE execution_id = ?
		AND operation = 'merge_pull_request')`, executionID).Scan(&found)
	return found, err
}

// InvalidateUnusedApprovalsTx invalidates the pending / approved approvals of
// the Execution with reason and audits each with actor system (spec 17 3.6,
// 3.9).
func InvalidateUnusedApprovalsTx(ctx context.Context, tx *sql.Tx, executionID, reason string) error {
	return invalidateOpenApprovalsTx(ctx, tx, executionID, reason)
}
