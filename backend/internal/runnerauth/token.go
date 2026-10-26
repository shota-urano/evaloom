// Package runnerauth issues and verifies credentials owned by the Control Plane.
package runnerauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// ValidateRegistration runs before the CLI opens or creates the database.
func ValidateRegistration(id, name, root string) error {
	if id == "" || id == "." || id == ".." || !utf8.ValidString(id) || strings.ContainsAny(id, `/\`) || strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("runner: id must be a non-empty URL path segment without whitespace or control characters")
	}
	if strings.TrimSpace(name) == "" || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("runner: name must be non-empty and contain no control characters")
	}
	if !filepath.IsAbs(root) || strings.ContainsRune(root, 0) {
		return fmt.Errorf("runner: worktree root must be an absolute path")
	}
	return nil
}

// Register returns the plaintext only after insertion succeeds. The caller must
// display it once, and must never log the token or persist it in the database.
func Register(ctx context.Context, db *sql.DB, id, name, root string) (string, error) {
	if err := ValidateRegistration(id, name, root); err != nil {
		return "", err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("runner: generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	if err := store.RegisterRunner(ctx, db, id, name, tokenHash(token), filepath.Clean(root), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	return token, nil
}

func tokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

// Authenticate resolves a canonical 256-bit bearer token on every request, so
// a removed/replaced database credential is never accepted from a memory cache.
func Authenticate(ctx context.Context, db *sql.DB, token string) (string, error) {
	secret, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != token {
		return "", sql.ErrNoRows
	}
	return store.RunnerIDByTokenHash(ctx, db, tokenHash(token))
}
