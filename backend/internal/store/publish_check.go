package store

import (
	"context"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// PublishCheckState is what POST /api/executions/{id}/publish-check reads
// (spec 17 3.7.1 steps 1 and 2) in one statement.
type PublishCheckState struct {
	RunnerID        string
	Status          model.ExecutionStatus
	Latest          bool
	CancelRequested bool
	// MergePullRequest is direct_push only when the stored policy says so.
	// Any other or unreadable value is the default require_approval (3.2),
	// so an unexpected value never allows a direct push.
	MergePullRequest string
}

// GetPublishCheckState returns sql.ErrNoRows when the Execution is absent.
func GetPublishCheckState(ctx context.Context, reader projectReader, executionID string) (PublishCheckState, error) {
	var state PublishCheckState
	var direct bool
	err := reader.QueryRowContext(ctx, `SELECT coalesce(e.runner_id, ''), e.status,
		e.seq = (SELECT max(seq) FROM executions WHERE task_id = e.task_id),
		e.cancel_requested_at IS NOT NULL,
		coalesce(CASE WHEN json_valid(p.approval_policy) THEN
			json_extract(p.approval_policy, '$.mergePullRequest') IS 'direct_push' AND json_extract(p.approval_policy, '$.version') IS 1 END, 0)
		FROM executions e JOIN tasks t ON t.id = e.task_id JOIN projects p ON p.id = t.project_id
		WHERE e.id = ?`, executionID).Scan(&state.RunnerID, &state.Status, &state.Latest, &state.CancelRequested, &direct)
	state.MergePullRequest = "require_approval"
	if direct {
		state.MergePullRequest = "direct_push"
	}
	return state, err
}
