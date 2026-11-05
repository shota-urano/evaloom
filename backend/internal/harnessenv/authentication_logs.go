package harnessenv

import (
	"encoding/json"
	"io"
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

// RedactLog is called only after recovery, before Close clears the lease. CLI
// output must remain in memory until then: refreshed values are not known at
// launch. Failed/invalid recovery suppresses all untrusted output because its
// secrets cannot be enumerated safely. Trusted diagnostics are added afterwards.
func (a *Authentication) RedactLog(text string) string {
	if text == "" {
		return ""
	}
	if !a.logReady || a.root == nil {
		return logredact.Marker
	}
	secrets := append(authenticationSecrets(a.initial), a.logSecrets...)
	r := logredact.NewLiteralReader(strings.NewReader(text), secrets)
	data, _ := io.ReadAll(r) // strings.Reader cannot fail
	return string(data)
}
