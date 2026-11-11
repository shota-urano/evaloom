package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// approvalTarget is an opaque approvals.target used by the owner API tests.
const approvalTarget = `{"name":"example"}`

func stageExec(t *testing.T, f *authFixture, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func stageCount(t *testing.T, f *authFixture, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// openApproval moves a publishing Execution to waiting_approval with one
// pending approval bound to contentVersion, in one transaction.
func openApproval(t *testing.T, f *authFixture, executionID, contentVersion string) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var taskID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM executions WHERE id = ?`, executionID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO approvals (id, task_id, execution_id, operation, target,
		content_version, params_hash, status, created_at)
		VALUES (?, ?, ?, 'merge_pull_request', ?, ?, ?, 'pending', '2026-11-08T00:00:00Z')`,
		ulid.Make().String(), taskID, executionID, approvalTarget, contentVersion, "hash-"+contentVersion); err != nil {
		t.Fatal(err)
	}
	if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
		TaskID: taskID, ExecutionID: executionID, FromTask: model.TaskRunning,
		FromExecution: model.ExecutionPublishing, ToExecution: model.ExecutionWaitingApproval,
	}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"from": "publishing", "to": "waiting_approval", "reason": "approval_required"})
	if _, err := store.AppendExecutionEvent(ctx, tx, executionID, model.EventStatusChanged, payload); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

type approvalRow struct {
	ID, TaskID, ExecutionID, Operation, Target, ContentVersion, ParamsHash, Status, Reason string
	MaxUses, UseCount                                                                      int
	Undecided                                                                              bool
}

func approvalsOf(t *testing.T, f *authFixture, taskID string) []approvalRow {
	t.Helper()
	rows, err := f.db.Query(`SELECT id, task_id, execution_id, operation, target, content_version, params_hash, status,
		coalesce(invalidated_reason, ''), max_uses, use_count,
		decided_by IS NULL AND decided_at IS NULL AND expires_at IS NULL AND consumed_at IS NULL AND result IS NULL
		FROM approvals WHERE task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []approvalRow
	for rows.Next() {
		var r approvalRow
		if err := rows.Scan(&r.ID, &r.TaskID, &r.ExecutionID, &r.Operation, &r.Target, &r.ContentVersion, &r.ParamsHash,
			&r.Status, &r.Reason, &r.MaxUses, &r.UseCount, &r.Undecided); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// requirePendingApproval checks the binding of a pending approval.
func requirePendingApproval(t *testing.T, r approvalRow, taskID, executionID, contentVersion string) {
	t.Helper()
	if r.TaskID != taskID || r.ExecutionID != executionID || r.Target != approvalTarget || r.ContentVersion != contentVersion ||
		r.Status != "pending" || r.Reason != "" || r.MaxUses != 1 || r.UseCount != 0 || !r.Undecided {
		t.Fatalf("approval = %+v; want pending approval bound to %s", r, contentVersion)
	}
}

func requireWaitingApproval(t *testing.T, f *authFixture, id string) {
	t.Helper()
	var execution, taskStatus string
	var finished bool
	if err := f.db.QueryRow(`SELECT e.status, t.status, e.finished_at IS NOT NULL
		FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, id).Scan(&execution, &taskStatus, &finished); err != nil {
		t.Fatal(err)
	}
	if execution != "waiting_approval" || taskStatus != "waiting_approval" || finished {
		t.Fatalf("execution=%s task=%s finished=%v; want waiting_approval without finishing", execution, taskStatus, finished)
	}
}
