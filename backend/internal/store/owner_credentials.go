package store

import (
	"context"
	"database/sql"
	"fmt"
)

// SetOwnerCredentials accepts only a derived password hash. Replacing the
// singleton (including its login ID), revoking sessions and recording the audit
// entry are atomic. The original creation time survives a reset (spec 18 3.3).
func SetOwnerCredentials(ctx context.Context, db *sql.DB, id, passwordHash string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := recordTime()
	result, err := tx.ExecContext(ctx, `UPDATE owner_credentials SET id = ?, password_hash = ?, updated_at = ?`, id, passwordHash, now)
	if err != nil {
		return fmt.Errorf("store: update owner credentials: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO owner_credentials (id, password_hash, created_at, updated_at)
			VALUES (?, ?, ?, ?)`, id, passwordHash, now, now); err != nil {
			return fmt.Errorf("store: insert owner credentials: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM owner_sessions`); err != nil {
		return fmt.Errorf("store: revoke owner sessions: %w", err)
	}
	if _, err := RecordAuditLog(ctx, tx, AuditLog{
		Actor: "owner", Action: "owner.set_password", TargetType: "owner_credentials", TargetID: id,
	}); err != nil {
		return fmt.Errorf("store: audit owner credentials: %w", err)
	}
	return tx.Commit()
}
