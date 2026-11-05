package claudecode

import (
	"bytes"
	"encoding/json"
	"sync"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

// lineWriter supports fragmented writes, long lines and a final line without LF.
// Collection does not wait for a Logs consumer. Output stays in memory until
// authentication recovery permits exclusion before Logs delivery.
type lineWriter struct {
	mu      sync.Mutex
	pending []byte
	session *session
	stream  string
	events  *events
}

func (w *lineWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, data...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		w.line(w.pending[:i])
		w.pending = w.pending[i+1:]
	}
	return len(data), nil
}

func (w *lineWriter) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 {
		w.line(w.pending)
		w.pending = nil
	}
}

func (w *lineWriter) line(data []byte) {
	text := string(bytes.TrimSuffix(data, []byte{'\r'}))
	w.session.mu.Lock()
	w.session.logs = append(w.session.logs, adapter.LogLine{Stream: w.stream, Text: text})
	w.session.mu.Unlock()
	select {
	case w.session.wake <- struct{}{}:
	default:
	}
	if w.events != nil {
		w.events.read([]byte(text))
	}
}

type events struct {
	completed, failed, invalid bool
	resultIndex                *int64
	summary                    string
	input, output              *int64
}

// Only the latest result owns summary and usage. Background work can start more
// turns after a result; result_index distinguishes them from duplicate output.
// Assistant messages cannot establish a successful run.
func (e *events) read(line []byte) {
	var event struct {
		Type        string  `json:"type"`
		Subtype     string  `json:"subtype"`
		IsError     *bool   `json:"is_error"`
		Result      *string `json:"result"`
		ResultIndex *int64  `json:"result_index"`
		Usage       *struct {
			Input  *int64 `json:"input_tokens"`
			Output *int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(line, &event) != nil || event.Type == "" {
		e.invalid = true
		return
	}
	switch event.Type {
	case "result":
		// Older CLIs omit the index: accept one such result. Multiple results
		// need explicit, increasing indices. Gaps are allowed because failed
		// writes still consume an index in Claude Code's delivery order.
		if event.Subtype == "" || event.IsError == nil ||
			(event.ResultIndex != nil && *event.ResultIndex < 0) ||
			(e.completed && (e.resultIndex == nil || event.ResultIndex == nil || *event.ResultIndex <= *e.resultIndex)) {
			e.invalid = true
		}
		e.completed = true
		e.resultIndex = event.ResultIndex
		if event.Subtype != "success" || event.IsError == nil || *event.IsError {
			e.failed = true
		}
		e.summary, e.input, e.output = "", nil, nil
		if event.Result != nil {
			e.summary = *event.Result
		}
		if event.Usage != nil {
			e.input, e.output = event.Usage.Input, event.Usage.Output
			if e.input != nil && *e.input < 0 || e.output != nil && *e.output < 0 {
				e.invalid = true
			}
		}
	case "error":
		e.failed = true
	}
}
