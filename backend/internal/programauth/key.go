// Package programauth issues and verifies program client keys (spec 20 4.1,
// 4.3). The plaintext key exists only in the CLI's single output; the database
// keeps its SHA-256 hash.
package programauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// ValidateRegistration runs before the CLI opens the database.
func ValidateRegistration(id, name, projectID, harnessID string) error {
	if !segment(id) {
		return fmt.Errorf("client: id must be a non-empty URL path segment without whitespace or control characters")
	}
	if strings.TrimSpace(name) == "" || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("client: name must be non-empty and contain no control characters")
	}
	if (projectID == "") == (harnessID == "") {
		return fmt.Errorf("client: exactly one of -project or -harness is required")
	}
	return nil
}

func segment(id string) bool {
	return id != "" && id != "." && id != ".." && utf8.ValidString(id) && !strings.ContainsAny(id, `/\`) &&
		!strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// Register returns the plaintext key only after the hash is committed. The
// caller displays it once and never logs or stores it.
func Register(ctx context.Context, db *sql.DB, id, name, projectID, harnessID string) (string, error) {
	if err := ValidateRegistration(id, name, projectID, harnessID); err != nil {
		return "", err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("client: generate key: %w", err)
	}
	key := base64.RawURLEncoding.EncodeToString(secret)
	client := store.ProgramClient{ID: id, Name: name, ProjectID: projectID, HarnessID: harnessID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := store.RegisterProgramClient(ctx, db, client, Hash(key)); err != nil {
		return "", err
	}
	return key, nil
}

// Hash is the stored form of a key.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Authenticate resolves a canonical 256-bit key on every request, so a revoked
// key stops working at once. Malformed, unknown and revoked keys are
// sql.ErrNoRows; database failures stay errors.
func Authenticate(ctx context.Context, db *sql.DB, key string) (store.ProgramClient, error) {
	secret, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != key {
		return store.ProgramClient{}, sql.ErrNoRows
	}
	return store.ProgramClientByTokenHash(ctx, db, Hash(key))
}
