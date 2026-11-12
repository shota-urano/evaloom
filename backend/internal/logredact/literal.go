// Package logredact provides the shared literal exclusion used before log storage.
package logredact

import (
	"bufio"
	"io"
	"strings"
)

const Marker = "[REDACTED]"

type secretNode struct {
	next map[byte]*secretNode
	end  bool
}

// LiteralReader matches exact bytes across read chunks and line boundaries.
// At the same position the longest secret wins; empty secrets are ignored.
// Lookahead is bounded by the longest registered secret, not the log size.
type LiteralReader struct {
	source  *bufio.Reader
	root    secretNode
	pending string
	changed bool
}

func NewLiteralReader(source io.Reader, secrets []string) *LiteralReader {
	return newLiteralReader(source, secrets, true)
}

func newLiteralReader(source io.Reader, secrets []string, normalize bool) *LiteralReader {
	r := &LiteralReader{}
	maxSize := 1
	for _, secret := range secrets {
		if normalize {
			secret = strings.ReplaceAll(secret, "\r\n", "\n")
		}
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

func (r *LiteralReader) Read(p []byte) (int, error) {
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
				r.pending, r.changed = Marker, true
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

func (r *LiteralReader) Changed() bool { return r.changed }
