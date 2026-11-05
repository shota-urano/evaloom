package runner

import (
	"bufio"
	"regexp"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/logredact"
)

const redactionMarker = logredact.Marker

var (
	authorizationPattern = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization[\t ]*:)[^\r\n]*`)
	privateKeyPattern    = regexp.MustCompile(`-----BEGIN ((?:[A-Z0-9]+ )*PRIVATE KEY)-----`)
)

// patternReader removes header values and PEM blocks before literal matching.
// State belongs to one Read source, so stdout cannot close stderr's PEM block.
// A missing END marker suppresses the remainder of that source, including on
// read errors. Raw bytes are only held in memory, never spooled to disk.
type patternReader struct {
	source     *bufio.Reader
	pending    string
	err        error
	privateEnd string
	changed    bool
}

func (r *patternReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.pending == "" && r.err == nil {
		line, err := r.source.ReadString('\n')
		r.err = err
		// Match the bytes that will actually be persisted. Otherwise CRLF
		// normalization could reconstitute a multiline secret after matching.
		r.pending = r.redactLine(strings.ReplaceAll(line, "\r\n", "\n"))
	}
	if r.pending != "" {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	return 0, r.err
}

func (r *patternReader) redactLine(line string) string {
	var safe strings.Builder
	for line != "" {
		if r.privateEnd != "" {
			end := strings.Index(line, r.privateEnd)
			if end < 0 {
				// Preserve line boundaries without retaining any key material.
				if strings.HasSuffix(line, "\n") {
					safe.WriteByte('\n')
				}
				break
			}
			line = line[end+len(r.privateEnd):]
			r.privateEnd = ""
			continue
		}
		start := privateKeyPattern.FindStringSubmatchIndex(line)
		if start == nil {
			safe.WriteString(line)
			break
		}
		safe.WriteString(line[:start[0]])
		safe.WriteString(redactionMarker)
		r.privateEnd = "-----END " + line[start[2]:start[3]] + "-----"
		r.changed = true
		line = line[start[1]:]
	}
	text := safe.String()
	if authorizationPattern.MatchString(text) {
		r.changed = true
		text = authorizationPattern.ReplaceAllString(text, "${1} "+redactionMarker)
	}
	return text
}
