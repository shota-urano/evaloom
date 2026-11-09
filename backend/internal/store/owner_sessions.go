package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// OwnerCredentials returns the singleton login ID and password hash, or
// sql.ErrNoRows when the owner has not set a password yet.
func OwnerCredentials(ctx context.Context, db *sql.DB) (id, passwordHash string, err error) {
	err = db.QueryRowContext(ctx, `SELECT id, password_hash FROM owner_credentials`).Scan(&id, &passwordHash)
	return id, passwordHash, err
}

// OwnerSession holds only hashes of the session and CSRF values (spec 18 3.8).
type OwnerSession struct {
	ID, TokenHash, CSRFTokenHash, Via string
	CreatedAt, ExpiresAt              time.Time
}

func ownerSessionTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// CreateOwnerSession stores the session together with its login audit record.
// Expired rows are removed in the same transaction. Any failure rolls back
// everything, so a login never succeeds without its audit entry.
func CreateOwnerSession(ctx context.Context, db *sql.DB, session OwnerSession, audit AuditLog) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM owner_sessions WHERE expires_at <= ?`, ownerSessionTime(session.CreatedAt)); err != nil {
		return fmt.Errorf("store: remove expired owner sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO owner_sessions (id, token_hash, csrf_token_hash, created_at, expires_at, via)
		VALUES (?, ?, ?, ?, ?, ?)`, session.ID, session.TokenHash, session.CSRFTokenHash,
		ownerSessionTime(session.CreatedAt), ownerSessionTime(session.ExpiresAt), session.Via); err != nil {
		return fmt.Errorf("store: insert owner session: %w", err)
	}
	if _, err := RecordAuditLog(ctx, tx, audit); err != nil {
		return fmt.Errorf("store: audit owner login: %w", err)
	}
	return tx.Commit()
}

// DeleteOwnerSession revokes only the requesting device's session (spec 18 3.5).
func DeleteOwnerSession(ctx context.Context, db *sql.DB, id string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM owner_sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete owner session: %w", err)
	}
	return nil
}

// OwnerSessionByTokenHash returns sql.ErrNoRows for unknown values. Callers
// decide expiry so that the clock is injectable.
func OwnerSessionByTokenHash(ctx context.Context, db *sql.DB, tokenHash string) (OwnerSession, error) {
	var s OwnerSession
	var created, expires string
	err := db.QueryRowContext(ctx, `SELECT id, token_hash, csrf_token_hash, created_at, expires_at, via
		FROM owner_sessions WHERE token_hash = ?`, tokenHash).Scan(&s.ID, &s.TokenHash, &s.CSRFTokenHash, &created, &expires, &s.Via)
	if err != nil {
		return OwnerSession{}, err
	}
	if s.CreatedAt, err = time.Parse(time.RFC3339, created); err != nil {
		return OwnerSession{}, fmt.Errorf("store: owner session %q: invalid created_at", s.ID)
	}
	if s.ExpiresAt, err = time.Parse(time.RFC3339, expires); err != nil {
		return OwnerSession{}, fmt.Errorf("store: owner session %q: invalid expires_at", s.ID)
	}
	return s, nil
}
