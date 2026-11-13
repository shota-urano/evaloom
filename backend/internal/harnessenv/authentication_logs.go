package harnessenv

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/logredact"
)

var authenticationString = regexp.MustCompile(`"(?:\\.|[^"\\])*"`)

// authenticationSecrets includes the whole file and all nested string values,
// so formatting changes and diagnostics printing a bare token cannot bypass
// exclusion. JSON-escaped values and individual lines are excluded as well.
func authenticationSecrets(data []byte) []string {
	secrets := []string{string(data)}
	// Data has already passed ValidateAuthentication. Scan every string value
	// instead of unmarshalling a map, which would discard duplicate JSON keys.
	for _, span := range authenticationString.FindAllIndex(data, -1) {
		if strings.HasPrefix(strings.TrimSpace(string(data[span[1]:])), ":") {
			continue // an object key, rather than a credential value
		}
		raw := data[span[0]:span[1]]
		var value string
		if json.Unmarshal(raw, &value) != nil {
			continue
		}
		secrets = append(secrets, value, string(raw[1:len(raw)-1]))
		encoded, _ := json.Marshal(value)
		secrets = append(secrets, string(encoded[1:len(encoded)-1]))
		for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
			secrets = append(secrets, line)
		}
	}
	return secrets
}

// ReadAuthenticationValues supplies Runner's file collectors with the same
// exclusions as Adapter logs. Authentication writeback atomically replaces the
// file, so this read cannot see a partial refresh and does not wait for another
// execution's long-lived credential lock. Discard the values after the stage.
func ReadAuthenticationValues(store string) ([]string, error) {
	root, err := os.OpenRoot(filepath.Dir(store))
	if err != nil {
		return nil, authError("cannot open store")
	}
	defer root.Close()
	data, err := readAuthentication(root, filepath.Base(store), true)
	if err != nil {
		return nil, err
	}
	return authenticationSecrets(data), nil
}

// FilterLog excludes all values observed so far before a line enters the log
// queue. It is safe alongside command-boundary refreshes and Close. Failed
// recovery suppresses later lines entirely, including after a successful retry.
func (a *Authentication) FilterLog(text string) (string, bool) {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.logBlocked {
		return "", false
	}
	r := logredact.NewLiteralReader(strings.NewReader(text), a.logSecrets)
	data, _ := io.ReadAll(r) // strings.Reader cannot fail
	return string(data), true
}

// RedactLog applies the same exclusions to the final summary. When recovery
// failed, return a marker rather than exposing the untrusted summary.
func (a *Authentication) RedactLog(text string) string {
	if text == "" {
		return ""
	}
	filtered, ok := a.FilterLog(text)
	if !ok {
		return logredact.Marker
	}
	return filtered
}
