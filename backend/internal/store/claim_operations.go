package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// ClaimOperation is the "approved operation" job of a claim (spec 17 3.5.1
// step 2, the `operation` addition to spec 04 3.3.1): an approved, unused
// approval whose Execution is waiting for the merge.
type ClaimOperation struct {
	ApprovalID     string          `json:"approvalId"`
	Operation      string          `json:"operation"`
	Target         json.RawMessage `json:"target"`
	ContentVersion string          `json:"contentVersion"`
}

// ClaimOperationExecution identifies the merging Execution the operation
// belongs to. Its state does not change on claim.
type ClaimOperationExecution struct {
	ID     string `json:"id"`
	TaskID string `json:"taskId"`
}

// OperationClaim is the claim response for an approved operation. Project is
// the snapshot the Git Publisher needs to authenticate to the repository.
type OperationClaim struct {
	Execution ClaimOperationExecution `json:"execution"`
	Project   ClaimProject            `json:"project"`
	Operation *ClaimOperation         `json:"operation"`
}

// NextOperationClaim reads the oldest approved merge whose Execution is the
// latest, merging Execution run by this Runner (spec 17 3.5.1). Nil means
// none. It runs before the capacity check of NextClaim: a merging Execution
// does not occupy a slot (Q59). The claim changes nothing; consume (3.5.2)
// is the only place an approval is used, so handing the same operation out
// twice cannot merge twice.
func NextOperationClaim(ctx context.Context, tx *sql.Tx, runnerID string) (*OperationClaim, error) {
	var c OperationClaim
	var operation ClaimOperation
	var target, projectID string
	err := tx.QueryRowContext(ctx, `SELECT a.id, a.operation, a.target, a.content_version, e.id, e.task_id, t.project_id
		FROM approvals a JOIN executions e ON e.id = a.execution_id JOIN tasks t ON t.id = e.task_id
		JOIN projects p ON p.id = t.project_id
		WHERE a.status = 'approved' AND a.use_count < a.max_uses AND a.operation = 'merge_pull_request'
		AND e.status = 'merging' AND t.status = 'waiting_approval' AND e.runner_id = ? AND p.runner_id = ?
		AND e.seq = (SELECT max(seq) FROM executions WHERE task_id = e.task_id)
		ORDER BY a.decided_at, a.id LIMIT 1`, runnerID, runnerID).
		Scan(&operation.ApprovalID, &operation.Operation, &target, &operation.ContentVersion,
			&c.Execution.ID, &c.Execution.TaskID, &projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	operation.Target = json.RawMessage(target)
	c.Operation = &operation
	c.Project.Project, err = GetProject(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ErrMergeNotConsumed means a merge result was reported for an Execution
// without a consumed, unreported merge approval: no merge was allowed.
var ErrMergeNotConsumed = errors.New("store: merge result requires a consumed approval")

// HasConsumedMergeTx reports whether the Execution has a consumed merge
// approval whose result is not recorded yet: only then was a merge allowed.
func HasConsumedMergeTx(ctx context.Context, tx *sql.Tx, executionID string) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM approvals WHERE execution_id = ?
		AND operation = 'merge_pull_request' AND status = 'consumed' AND result IS NULL)`, executionID).Scan(&found)
	return found, err
}

// RecordMergeResultTx saves the merge outcome on the consumed approval of the
// Execution (spec 17 3.5.2 step 4, approvals.result). result is the JSON
// object stored in approvals.result. When invalidate is not empty the consumed
// approval is also marked invalidated with that reason (spec 17 6章: GitHub
// refused the merge because the head SHA changed) and audited with actor
// system. It returns the approval ID.
func RecordMergeResultTx(ctx context.Context, tx *sql.Tx, executionID string, result map[string]string, invalidate string) (string, error) {
	var id, taskID, operation string
	err := tx.QueryRowContext(ctx, `SELECT id, task_id, operation FROM approvals
		WHERE execution_id = ? AND operation = 'merge_pull_request' AND status = 'consumed' AND result IS NULL
		ORDER BY consumed_at DESC, id DESC LIMIT 1`, executionID).Scan(&id, &taskID, &operation)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrMergeNotConsumed
	}
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET result = ? WHERE id = ?`, string(encoded), id); err != nil {
		return "", err
	}
	if invalidate == "" {
		return id, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status = 'invalidated', invalidated_reason = ? WHERE id = ?`, invalidate, id); err != nil {
		return "", err
	}
	_, err = RecordAuditLog(ctx, tx, AuditLog{
		Actor: "system", Action: "approval.invalidate", TargetType: "approval", TargetID: id,
		Detail: map[string]any{"taskId": taskID, "executionId": executionID, "operation": operation, "reason": invalidate},
	})
	return id, err
}
