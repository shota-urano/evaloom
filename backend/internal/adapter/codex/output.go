package codex

import (
	"bytes"
	"encoding/json"
	"sync"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

// lineWriter supports fragmented writes, long lines and a final line without LF.
// Logging does not wait for a Logs consumer and never drops CLI output.
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
	summary                    string
	input, output              *int64
}

func (e *events) read(line []byte) {
	var event struct {
		Type string `json:"type"`
		Item struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		} `json:"item"`
		Usage *struct {
			Input  *int64 `json:"input_tokens"`
			Output *int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(line, &event) != nil || event.Type == "" {
		e.invalid = true
		return
	}
	switch event.Type {
	case "item.completed":
		if event.Item.Type == "agent_message" {
			if event.Item.Text == nil {
				e.invalid = true
			} else {
				e.summary = *event.Item.Text
			}
		}
	case "turn.completed":
		e.completed = true
		if event.Usage != nil {
			e.input, e.output = event.Usage.Input, event.Usage.Output
			if e.input != nil && *e.input < 0 || e.output != nil && *e.output < 0 {
				e.invalid = true
			}
		}
	case "turn.failed", "error":
		e.failed = true
	}
}
