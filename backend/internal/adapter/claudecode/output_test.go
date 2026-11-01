package claudecode

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

func TestClaudeCodeLogsPreserveFragmentedLongLines(t *testing.T) {
	s := &session{wake: make(chan struct{}, 1), done: make(chan struct{})}
	parser := &events{}
	w := &lineWriter{session: s, stream: "stdout", events: parser}
	summary := strings.Repeat("x", 128<<10)
	line := fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"result":%q}`, summary)
	for i := 0; i < len(line); i += 37 {
		if _, err := w.Write([]byte(line[i:min(i+37, len(line))])); err != nil {
			t.Fatal(err)
		}
	}
	w.Write([]byte("\r\n{\"type\":\"future.event\"}"))
	w.finish()
	if len(s.logs) != 2 || s.logs[0].Text != line || parser.summary != summary || !parser.completed || parser.invalid {
		t.Fatalf("logs=%d summary length=%d parser=%+v", len(s.logs), len(parser.summary), parser)
	}
}

func TestClaudeCodeConcurrentSessionsAndLateLogConsumer(t *testing.T) {
	f := newFixture(t, "success")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ids := make([]adapter.SessionID, 4)
	for i := range ids {
		id, err := f.h.Start(ctx, f.request, f.env)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	for _, id := range ids {
		// Result must finish even if Logs was never called.
		r, err := f.h.Result(id)
		if err != nil || r.Status != adapter.ResultSucceeded {
			t.Fatalf("result=%+v err=%v", r, err)
		}
		*r.Usage.InputTokens = 999
		again, err := f.h.Result(id)
		if err != nil || *again.Usage.InputTokens != 17 {
			t.Fatalf("mutable result=%+v err=%v", again, err)
		}
		lines, err := f.h.Logs(id)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for range lines {
			n++
		}
		if n < 4 {
			t.Fatalf("lost logs: %d", n)
		}
	}
}

func TestClaudeCodeTerminalOutputValidation(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		wantSuccess  bool
	}{
		{"success", `{"type":"assistant","message":{"usage":{"input_tokens":999},"content":[{"type":"text","text":"intermediate"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"final","usage":{"input_tokens":17,"output_tokens":9,"cache_read_input_tokens":5}}`, true},
		{"no_summary_or_usage", `{"type":"result","subtype":"success","is_error":false}`, true},
		{"is_error", `{"type":"result","subtype":"success","is_error":true}`, false},
		{"max_turns", `{"type":"result","subtype":"error_max_turns","is_error":false}`, false},
		{"missing_is_error", `{"type":"result","subtype":"success"}`, false},
		{"missing_subtype", `{"type":"result","is_error":false}`, false},
		{"invalid_usage", `{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":"17"}}`, false},
		{"negative_usage", `{"type":"result","subtype":"success","is_error":false,"usage":{"output_tokens":-1}}`, false},
		{"duplicate_result", `{"type":"result","subtype":"success","is_error":false}
{"type":"result","subtype":"success","is_error":false}`, false},
		{"invalid_json", `{"type":"result"`, false},
		{"no_result", `{"type":"assistant"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &events{}
			for _, line := range strings.Split(tc.output, "\n") {
				e.read([]byte(line))
			}
			if got := e.completed && !e.failed && !e.invalid; got != tc.wantSuccess {
				t.Fatalf("events=%+v, want success=%t", e, tc.wantSuccess)
			}
			if tc.name == "success" && (e.summary != "final" || e.input == nil || *e.input != 17 || e.output == nil || *e.output != 9) {
				t.Fatalf("summary/usage taken from intermediate message: %+v", e)
			}
		})
	}
}
