package runner

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

const redactionMarker = "[REDACTED]"

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

type secretNode struct {
	next map[byte]*secretNode
	end  bool
}

// secretReader matches exact bytes across read chunks and line boundaries.
// At the same position the longest secret wins; empty secrets are ignored.
// Lookahead is bounded by the longest registered secret, not the log size.
type secretReader struct {
	source  *bufio.Reader
	root    secretNode
	pending string
	changed bool
}

func newSecretReader(source io.Reader, secrets []string) *secretReader {
	r := &secretReader{}
	maxSize := 1
	for _, secret := range secrets {
		secret = strings.ReplaceAll(secret, "\r\n", "\n")
		if secret == "" {
			continue
		}
		maxSize = max(maxSize, len(secret))
		node := &r.root
		for i := range len(secret) {
			if node.next == nil {
				node.next = make(map[byte]*secretNode)
			}
			if node.next[secret[i]] == nil {
				node.next[secret[i]] = &secretNode{}
			}
			node = node.next[secret[i]]
		}
		node.end = true
	}
	r.source = bufio.NewReaderSize(source, maxSize)
	return r
}

func (r *secretReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if r.pending == "" {
			first, err := r.source.Peek(1)
			if err != nil {
				return n, err
			}
			firstByte := first[0]
			node, matched := &r.root, 0
			for length := 1; len(node.next) > 0; length++ {
				prefix, err := r.source.Peek(length)
				if err != nil {
					break
				}
				node = node.next[prefix[length-1]]
				if node == nil {
					break
				}
				if node.end {
					matched = length
				}
			}
			if matched > 0 {
				r.source.Discard(matched)
				r.pending, r.changed = redactionMarker, true
			} else {
				r.pending = string([]byte{firstByte})
				r.source.Discard(1)
			}
		}
		p[n] = r.pending[0]
		r.pending = r.pending[1:]
		n++
		// Return complete lines promptly while the producer is still running.
		if p[n-1] == '\n' {
			break
		}
	}
	return n, nil
}
