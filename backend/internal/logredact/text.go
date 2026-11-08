package logredact

import (
	"io"
	"strings"
)

// Text applies the same exclusion as Runner logs (spec 05 3.5) to one text:
// the well-known secret forms first, then exact matches of secrets. It
// reports whether anything was replaced. Line endings are normalized to LF,
// as in persisted logs.
func Text(text string, secrets []string) (string, bool) {
	patterns := NewPatternReader(strings.NewReader(text))
	literals := NewLiteralReader(patterns, secrets)
	data, _ := io.ReadAll(literals) // strings.Reader cannot fail
	return string(data), patterns.Changed() || literals.Changed()
}
