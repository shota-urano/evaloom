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

func TestClaudeCodeMultipleResults(t *testing.T) {
	result := func(index string) string {
		return `{"type":"result","subtype":"success","is_error":false` + index + `}`
	}
	first := `{"type":"result","subtype":"success","is_error":false,"result_index":0,"result":"intermediate","usage":{"input_tokens":17,"output_tokens":9}}`
	for _, tc := range []struct {
		name, output string
		wantSuccess  bool
	}{
		{"increasing", result(`,"result_index":0`) + "\n" + result(`,"result_index":1`), true},
		// A failed write consumes its index in Claude Code; gaps are valid.
		{"gap", first + "\n" + result(`,"result_index":2`), true},
		{"legacy_single", result(""), true},
		{"duplicate_index", first + "\n" + result(`,"result_index":0`), false},
		{"repeated_earlier_index", first + "\n" + result(`,"result_index":1`) + "\n" + result(`,"result_index":0`), false},
		{"reverse_order", result(`,"result_index":2`) + "\n" + result(`,"result_index":1`), false},
		{"missing_first_index", result("") + "\n" + result(`,"result_index":1`), false},
		{"missing_later_index", first + "\n" + result(""), false},
		{"negative_index", result(`,"result_index":-1`), false},
		{"fractional_index", result(`,"result_index":0.5`), false},
		{"string_index", result(`,"result_index":"0"`), false},
		{"invalid_later_result", first + "\n" + `{"type":"result","result_index":1}`, false},
		{"invalid_then_valid", first + "\nnot json\n" + result(`,"result_index":1`), false},
		{"failed_final_result", first + "\n" + `{"type":"result","subtype":"error_during_execution","is_error":true,"result_index":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &events{}
			for _, line := range strings.Split(tc.output, "\n") {
				e.read([]byte(line))
			}
			if got := e.completed && !e.failed && !e.invalid; got != tc.wantSuccess {
				t.Fatalf("events=%+v, want success=%t", e, tc.wantSuccess)
			}
			if tc.wantSuccess && (e.summary != "" || e.input != nil || e.output != nil) {
				t.Fatalf("retained summary/usage from an earlier result: %+v", e)
			}
		})
	}
}

func TestClaudeCodeMultipleResultsFromCLI(t *testing.T) {
	for _, mode := range []string{"multiple-results", "multiple-results-no-fields", "multiple-results-failed", "duplicate-result-index"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, mode)
			_, r, logs := runResult(t, f)
			want := adapter.ResultSucceeded
			if mode == "multiple-results-failed" || mode == "duplicate-result-index" {
				want = adapter.ResultFailed
			}
			if r.Status != want || (want == adapter.ResultSucceeded && r.ErrorClass != nil) ||
				(want == adapter.ResultFailed && (r.ErrorClass == nil || *r.ErrorClass != adapter.ErrorExecutionFailed)) {
				t.Fatalf("result=%+v logs=%+v", r, logs)
			}
			switch mode {
			case "multiple-results":
				if r.Summary != "final summary" || r.Usage.InputTokens == nil || *r.Usage.InputTokens != 17 || r.Usage.OutputTokens == nil || *r.Usage.OutputTokens != 9 {
					t.Fatalf("final summary/usage lost or summed: %+v", r)
				}
			case "multiple-results-no-fields", "multiple-results-failed":
				if r.Summary != "" || r.Usage.InputTokens != nil || r.Usage.OutputTokens != nil {
					t.Fatalf("retained summary/usage from an earlier result: %+v", r)
				}
			case "duplicate-result-index":
				if r.Usage.InputTokens != nil || r.Usage.OutputTokens != nil {
					t.Fatalf("invalid output retained usage: %+v", r)
				}
			}
			results := 0
			for _, line := range logs {
				if line.Stream == "stdout" && strings.Contains(line.Text, `"type":"result"`) {
					results++
				}
			}
			if results != 2 {
				t.Fatalf("got %d result logs, want 2: %+v", results, logs)
			}
		})
	}
}
