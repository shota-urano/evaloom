// Package ownerauth handles the owner's password credentials (spec 18 3.3).
package ownerauth

import (
	"crypto/rand"
	"crypto/subtle"
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

// VerifyPassword reports whether password matches a PHC string produced by
// HashPassword. The comparison of derived keys is constant time. A malformed
// stored hash is an error, never a successful verification.
func VerifyPassword(password []byte, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, fmt.Errorf("owner: stored password hash is malformed")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, fmt.Errorf("owner: stored password hash has an unsupported version")
	}
	var memory, time uint32
	var threads uint8
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil || n != 3 || memory == 0 || time == 0 || threads == 0 {
		return false, fmt.Errorf("owner: stored password hash has invalid parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false, fmt.Errorf("owner: stored password hash has an invalid salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, fmt.Errorf("owner: stored password hash has an invalid key")
	}
	got := argon2.IDKey(password, salt, time, memory, threads, uint32(len(want)))
	defer clear(got)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
