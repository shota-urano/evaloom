package ownerauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

// SessionLifetime is fixed from login; use does not extend it (spec 18 3.5, Q92).
const SessionLifetime = 3 * 24 * time.Hour

// NewSessionToken returns 32 bytes from a CSPRNG as unpadded base64url.
// The value is a credential: store only TokenHash and never log it.
func NewSessionToken() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("owner: generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}

// CanonicalSessionToken rejects values that NewSessionToken cannot produce
// before they reach the database.
func CanonicalSessionToken(token string) bool {
	secret, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(secret) == 32 && base64.RawURLEncoding.EncodeToString(secret) == token
}

// TokenHash is the lowercase hex SHA-256 stored in owner_sessions.
func TokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

// CSRFToken derives the per-session CSRF token from the session value, so the
// database keeps only hashes while GET /api/session can still return the token
// to the holder of the HttpOnly cookie.
func CSRFToken(session string) string {
	mac := hmac.New(sha256.New, []byte(session))
	mac.Write([]byte("evaloom owner csrf v1"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
