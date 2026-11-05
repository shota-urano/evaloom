package codex

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

func TestCodexLogsPreserveFragmentedLongLines(t *testing.T) {
	s := &session{wake: make(chan struct{}, 1), done: make(chan struct{})}
	parser := &events{}
	w := &lineWriter{session: s, stream: "stdout", events: parser}
	summary := strings.Repeat("x", 128<<10)
	line := fmt.Sprintf(`{"type":"item.completed","item":{"type":"agent_message","text":%q}}`, summary)
	for i := 0; i < len(line); i += 37 {
		if _, err := w.Write([]byte(line[i:min(i+37, len(line))])); err != nil {
			t.Fatal(err)
		}
	}
	w.Write([]byte("\r\n{\"type\":\"turn.completed\"}"))
	w.finish()
	if len(s.logs) != 2 || s.logs[0].Text != line || parser.summary != summary || !parser.completed || parser.invalid {
		t.Fatalf("logs=%d summary length=%d parser=%+v", len(s.logs), len(parser.summary), parser)
	}
}

func TestCodexConcurrentSessionsAndLateLogConsumer(t *testing.T) {
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
			logs, _ := f.h.Logs(id)
			var diagnostic []string
			for line := range logs {
				diagnostic = append(diagnostic, line.Text)
			}
			t.Fatalf("result=%+v err=%v class=%v logs=%v", r, err, r.ErrorClass, diagnostic)
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
		if n < 5 {
			t.Fatalf("lost logs: %d", n)
		}
	}
}
