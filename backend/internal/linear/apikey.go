// Package linear is the Control Plane's Linear GraphQL client (spec 15, 3.1,
// 3.2 and 5). It only reads Issues; it never reads comments or attachments.
// The API key value is held only for the duration of one request and never
// appears in errors, logs or values meant for the database.
package linear

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// APIKeyFileName is the key file inside ~/.config/evaloom (spec 15 3.1, Q49).
const APIKeyFileName = "linear-api-key"

// apiKeyMaxBytes bounds the read of the key file; a Linear API key is far
// shorter. It is a read guard, not a configuration value.
const apiKeyMaxBytes = 4096

// KeyProblem says why the API key could not be used. It never carries the key.
type KeyProblem string

const (
	KeyMissing     KeyProblem = "api_key_missing"
	KeyPermission  KeyProblem = "api_key_permission_not_0600"
	KeyNotRegular  KeyProblem = "api_key_not_regular_file"
	KeyUnreadable  KeyProblem = "api_key_unreadable"
	KeyEmpty       KeyProblem = "api_key_empty"
	KeyInvalidForm KeyProblem = "api_key_invalid_characters"
)

// KeyError is returned when the key file cannot be used. Queries are not
// started in that case (task evaloom-iao.3 acceptance criteria).
type KeyError struct{ Problem KeyProblem }

func (e *KeyError) Error() string { return "linear: API key unusable: " + string(e.Problem) }

// DefaultAPIKeyPath returns ~/.config/evaloom/linear-api-key.
func DefaultAPIKeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", errors.New("linear: cannot determine home directory")
	}
	return filepath.Join(home, ".config", "evaloom", APIKeyFileName), nil
}

// ReadAPIKey reads the key file. The file must be a regular file with mode
// exactly 0600 (spec 15 3.1). Permissions are checked on the opened file so
// the checked file is the file that is read.
func ReadAPIKey(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", &KeyError{KeyMissing}
		}
		return "", &KeyError{KeyUnreadable}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", &KeyError{KeyUnreadable}
	}
	if !info.Mode().IsRegular() {
		return "", &KeyError{KeyNotRegular}
	}
	if info.Mode().Perm() != 0o600 {
		return "", &KeyError{KeyPermission}
	}
	data, err := io.ReadAll(io.LimitReader(f, apiKeyMaxBytes+1))
	if err != nil || len(data) > apiKeyMaxBytes {
		return "", &KeyError{KeyUnreadable}
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", &KeyError{KeyEmpty}
	}
	for _, r := range key {
		// A header value must be visible ASCII; anything else would be
		// rejected by net/http or would split the header.
		if r <= ' ' || r > '~' {
			return "", &KeyError{KeyInvalidForm}
		}
	}
	return key, nil
}
