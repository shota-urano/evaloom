package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestStageChannelFailsClosedWhenControlPlaneStops(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"allowed":true}`)
	}))
	defer server.Close()
	runner, err := New(testConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	outcome := runner.runStageWithChannel(t.Context(), ulid.Make().String(),
		StageInvocation{Stage: model.StagePublish, Directory: channelDirectory(t)},
		stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
			conn := channelConnection(t, in.ChannelPath)
			request := `{"op":"authorize","operation":"publish","approvalId":null,"contentId":"r1"}`
			if got := channelReply(t, conn, request); got != `{"allowed":true}` {
				t.Fatalf("live Control Plane reply=%s", got)
			}
			server.Close()
			if got := channelReply(t, conn, request); got != `{"allowed":false,"reason":"unavailable"}` {
				t.Fatalf("stopped Control Plane reply=%s", got)
			}
			// An unavailable authorization must not disable local cancellation
			// checks or require a new stage/channel connection.
			if got := channelReply(t, conn, `{"op":"cancel_check"}`); got != `{"cancelRequested":false}` {
				t.Fatalf("local cancel_check reply=%s", got)
			}
			return successfulStage(in.Stage)
		}))
	if outcome.RunnerError != nil {
		t.Fatalf("stage failed: %+v", outcome)
	}
}

func TestStageChannelRejectsValidJSONBeyondLineLimit(t *testing.T) {
	for _, ending := range []string{"\n", ""} {
		name := "terminated"
		if ending == "" {
			name = "unterminated"
		}
		t.Run(name, func(t *testing.T) {
			runner := &Runner{dataDir: t.TempDir()}
			channel, err := runner.openStageChannel(t.Context(), ulid.Make().String(), model.StageCheck,
				filepath.Join(channelDirectory(t), "s"))
			if err != nil {
				t.Fatal(err)
			}
			defer channel.close()
			conn := channelConnection(t, channel.listener.Addr().String())
			request := `{"op":"cancel_check"}`
			line := request + strings.Repeat(" ", channelLineLimit+1-len(request))
			if !json.Valid([]byte(line)) || len(line) != 64*1024+1 {
				t.Fatal("fixture must be valid JSON exactly one byte over the limit")
			}
			if _, err := io.WriteString(conn, line+ending); err != nil {
				t.Fatal(err)
			}
			// Keep the writing half open: reject oversize input even when the
			// peer sends neither LF nor EOF, rather than buffering indefinitely.
			requireChannelEOF(t, conn)
		})
	}
}
