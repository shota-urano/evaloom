package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
)

// Approval is one approvals row (spec 17 3.9) as the owner APIs show it.
type Approval struct {
	ID, TaskID, ExecutionID, Operation      string
	Target                                  json.RawMessage
	ContentVersion, ParamsHash, Status      string
	InvalidatedReason, DecidedBy, DecidedAt *string
	ExpiresAt, ConsumedAt                   *string
	Result                                  json.RawMessage
	MaxUses, UseCount                       int
	CreatedAt                               string
}

// ApprovalDetail is what the owner reviews before deciding (spec 17 3.4).
type ApprovalDetail struct {
	Approval
}

// Preserve the existing owner API view while reading the generic schema.
// Decisions are made by the owner; result is no longer an approvals column.
const approvalColumns = `id, task_id, execution_id, operation, target, content_id, params_hash, status,
	invalidated_reason, CASE WHEN decided_at IS NOT NULL THEN 'owner' END, decided_at,
	expires_at, consumed_at, NULL, max_uses, use_count, created_at`

type rowScanner interface{ Scan(...any) error }

func scanApproval(row rowScanner) (Approval, error) {
	var a Approval
	var target string
	var result sql.NullString
	err := row.Scan(&a.ID, &a.TaskID, &a.ExecutionID, &a.Operation, &target, &a.ContentVersion, &a.ParamsHash, &a.Status,
		&a.InvalidatedReason, &a.DecidedBy, &a.DecidedAt, &a.ExpiresAt, &a.ConsumedAt, &result,
		&a.MaxUses, &a.UseCount, &a.CreatedAt)
	if err != nil {
		return a, err
	}
	a.Target = json.RawMessage(target)
	if result.Valid {
		a.Result = json.RawMessage(result.String)
	}
	return a, nil
}

// GetApprovalTx reads one approval. sql.ErrNoRows means it does not exist.
func GetApprovalTx(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (Approval, error) {
	return scanApproval(reader.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id))
}

// ListApprovals returns approvals newest first (IDs are ULIDs), optionally
// filtered by status, strictly before the before ID when it is not empty.
func ListApprovals(ctx context.Context, db *sql.DB, status, before string, limit int) ([]Approval, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+approvalColumns+` FROM approvals
		WHERE (? = '' OR status = ?) AND (? = '' OR id < ?) ORDER BY id DESC LIMIT ?`,
		status, status, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetApprovalDetail reads the approval in one snapshot. sql.ErrNoRows means no approval.
func GetApprovalDetail(ctx context.Context, db *sql.DB, id string) (ApprovalDetail, error) {
	var detail ApprovalDetail
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return detail, err
	}
	defer tx.Rollback()
	detail.Approval, err = GetApprovalTx(ctx, tx, id)
	if err != nil {
		return detail, err
	}
	return detail, tx.Commit()
}

// ErrApprovalStale means the approval is no longer in the state the caller
// read (decided, invalidated, or expired by another request).
var ErrApprovalStale = errors.New("store: approval changed before it could be updated")

// DecideApprovalTx records the owner's decision on a pending approval bound
// to contentVersion (spec 17 3.4). expiresAt is set only for approvals. The
// update is conditional on the expected current values, so a concurrent
// decision or invalidation yields ErrApprovalStale.
func DecideApprovalTx(ctx context.Context, tx *sql.Tx, id, contentVersion, status string, expiresAt *string, now string) error {
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status = ?, decided_at = ?, expires_at = ?
		WHERE id = ? AND status = 'pending' AND content_id = ?`, status, now, expiresAt, id, contentVersion)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrApprovalStale
	}
	action := "approve"
	if status == "rejected" {
		action = "reject"
	}
	answer, err := json.Marshal(map[string]string{"action": action, "contentId": contentVersion})
	if err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE owner_decisions SET status='answered',answer=?,answered_at=?
		WHERE id=(SELECT decision_id FROM approvals WHERE id=?) AND status='open'`, string(answer), now, id)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrApprovalStale
	}
	return nil
}

// ApprovalExpired reports whether an approval is past its validity: status
// expired, or approved with expires_at at or before now (spec 17 3.6).
func ApprovalExpired(a Approval, now time.Time) bool {
	if a.Status == "expired" {
		return true
	}
	if a.Status != "approved" || a.ExpiresAt == nil {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, *a.ExpiresAt)
	return err == nil && !expires.After(now)
}

// RenewApprovalTx implements POST /api/approvals/{id}/renew (spec 17 3.6):
// an approved approval past expires_at is first marked expired, then a new
// pending approval with the same binding (Execution, operation, target,
// content_version, params_hash) is inserted. The caller has checked that
// the approval is expired and owns the transaction.
func RenewApprovalTx(ctx context.Context, tx *sql.Tx, old Approval, now string) (string, error) {
	if old.Status == "approved" {
		result, err := tx.ExecContext(ctx, `UPDATE approvals SET status = 'expired'
			WHERE id = ? AND status = 'approved' AND use_count = 0 AND julianday(expires_at) <= julianday(?)`, old.ID, now)
		if err != nil {
			return "", err
		}
		if n, err := result.RowsAffected(); err != nil {
			return "", err
		} else if n != 1 {
			return "", ErrApprovalStale
		}
	}
	id, decisionID := ulid.Make().String(), ulid.Make().String()
	if _, err := tx.ExecContext(ctx, `UPDATE owner_decisions SET status='withdrawn',withdrawn_reason='expired'
		WHERE id=(SELECT decision_id FROM approvals WHERE id=?) AND status='open'`, old.ID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO owner_decisions
		(id,kind,task_id,execution_id,subject_id,basis,detail,status,created_at)
		SELECT ?,'approval',a.task_id,a.execution_id,?,a.content_id,d.detail,'open',?
		FROM approvals a JOIN owner_decisions d ON d.id=a.decision_id WHERE a.id=? AND a.status='expired'`,
		decisionID, id, now, old.ID); err != nil {
		return "", err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO approvals (id, task_id, execution_id, decision_id, requested_stage, operation, target,
		summary, content_id, params_hash, status, max_uses, use_count, created_at)
		SELECT ?, task_id, execution_id, ?, requested_stage, operation, target, summary, content_id, params_hash, 'pending', max_uses, 0, ?
		FROM approvals WHERE id = ? AND status = 'expired'`, id, decisionID, now, old.ID)
	if err != nil {
		return "", err
	}
	if n, err := result.RowsAffected(); err != nil {
		return "", err
	} else if n != 1 {
		return "", ErrApprovalStale
	}
	return id, nil
}
