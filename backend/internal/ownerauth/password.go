// Package ownerauth handles the owner's password credentials (spec 18 3.3).
package ownerauth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Q90 delegates the minimum length to implementation. Count Unicode code
// points, preserve whitespace and impose no character composition rules.
const MinPasswordLength = 15

// RFC 9106 section 4, second recommended Argon2id configuration.
const (
	passwordMemory  = 64 * 1024 // KiB
	passwordTime    = 3
	passwordThreads = 4
	passwordSaltLen = 16
	passwordKeyLen  = 32
)

func ValidateID(id string) error {
	if strings.TrimSpace(id) == "" || !utf8.ValidString(id) || strings.ContainsFunc(id, unicode.IsControl) {
		return fmt.Errorf("owner: id must be non-empty UTF-8 text without control characters")
	}
	return nil
}

// HashPassword produces a self-contained PHC string with a fresh random salt.
// Neither errors nor logs include the password or hash. The caller owns and
// clears the input buffer after use.
func HashPassword(password []byte) (string, error) {
	if !utf8.Valid(password) || utf8.RuneCount(password) < MinPasswordLength {
		return "", fmt.Errorf("owner: password must contain at least %d UTF-8 characters", MinPasswordLength)
	}
	salt := make([]byte, passwordSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("owner: generate password salt: %w", err)
	}
	key := argon2.IDKey(password, salt, passwordTime, passwordMemory, passwordThreads, passwordKeyLen)
	defer clear(key)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		passwordMemory, passwordTime, passwordThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}
