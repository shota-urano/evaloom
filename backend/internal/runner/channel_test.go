package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

func channelDirectory(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ec-") // below macOS's Unix socket path limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Profile extraction freezes directories; make our own fixture writable
		// before removing it, just as newProfileRunner's ordinary cleanup does.
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
		_ = os.RemoveAll(dir)
	})
	return dir
}

func channelConnection(t *testing.T, path string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func channelReply(t *testing.T, conn net.Conn, line string) string {
	t.Helper()
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		t.Fatal(err)
	}
	data, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(data, "\n")
}

func requireChannelEOF(t *testing.T, conn net.Conn) {
	t.Helper()
	data := make([]byte, 1)
	n, err := conn.Read(data)
	if n != 0 || err != io.EOF {
		t.Fatalf("channel responded or remained open: n=%d err=%v", n, err)
	}
}

func TestStageChannelPublishRelaysToRunnerAPI(t *testing.T) {
	for _, response := range []string{`{"allowed":true}`, `{"allowed":false,"reason":"already_used"}`,
		`{"allowed":false,"reason":"content_changed"}`, `{"allowed":null}`, `{"allowed":true} {}`} {
		t.Run(response, func(t *testing.T) {
			id, approval := ulid.Make().String(), ulid.Make().String()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/api/executions/"+id+"/authorize" || r.Method != "POST" ||
					r.Header.Get("Authorization") != "Bearer "+testToken {
					t.Error("incorrect relay path, method or Runner credentials")
				}
				var input contract.AuthorizeRequest
				if json.NewDecoder(r.Body).Decode(&input) != nil || input.Operation != "merge_pull_request" ||
					input.ContentID != "opaque:revision-1" || input.ApprovalID == nil || *input.ApprovalID != approval {
					t.Errorf("incorrect relay payload: %+v", input)
				}
				io.WriteString(w, response)
			}))
			defer server.Close()
			runner, err := New(testConfig(t, server.URL))
			if err != nil {
				t.Fatal(err)
			}
			var conn net.Conn
			var path string
			outcome := runner.runStageWithChannel(context.Background(), id,
				StageInvocation{Stage: model.StagePublish, Directory: channelDirectory(t)},
				stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
					path = in.ChannelPath
					conn = channelConnection(t, path)
					request := `{"op":"authorize","operation":"merge_pull_request","approvalId":"` + approval + `","contentId":"opaque:revision-1"}`
					got := channelReply(t, conn, request)
					want := response
					if strings.Contains(response, "null") || strings.HasSuffix(response, "{}") {
						want = `{"allowed":false,"reason":"unavailable"}`
					}
					if got != want || strings.Contains(got, testToken) {
						t.Fatalf("reply=%s want=%s", got, want)
					}
					return successfulStage(in.Stage)
				}))
			if outcome.RunnerError != nil || calls.Load() != 1 {
				t.Fatalf("outcome=%+v HTTP calls=%d", outcome, calls.Load())
			}
			requireChannelEOF(t, conn)
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("finished stage socket remains: %v", err)
			}
		})
	}
}

func TestStageChannelRejectsForbiddenAndMalformedRequests(t *testing.T) {
	validAuthorize := `{"op":"authorize","operation":"publish","approvalId":null,"contentId":"r1"}`
	for _, stage := range []model.Stage{model.StagePrepare, model.StageCheck, model.StagePublish} {
		t.Run(string(stage), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			runner, err := New(testConfig(t, server.URL))
			if err != nil {
				t.Fatal(err)
			}
			channel, err := runner.openStageChannel(context.Background(), ulid.Make().String(), stage, filepath.Join(channelDirectory(t), "s"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(channel.close)
			requests := []string{
				`{}`, `null`, `[]`, `{"op":null}`, `{"Op":"cancel_check"}`,
				`{"op":"cancel_check","op":"cancel_check"}`, `{"op":"cancel_check","extra":1}`,
				`{"op":"cancel_check"} {}`, `{"op":"delete"}`, `{"op":"authorize"}`,
				`{"op":"authorize","operation":"publish","contentId":"r1"}`,
				`{"op":"authorize","operation":"publish","approvalId":null,"contentId":null}`,
				`{"op":"authorize","operation":"publish","approvalId":null,"contentId":"r1","executionId":"other"}`,
				`{"op":"authorize","operation":"publish","approvalId":null,"contentId":"r1","contentId":"r2"}`,
				`{"op":"authorize","operation":"bad-operation","approvalId":null,"contentId":"r1"}`,
				strings.Repeat(" ", channelLineLimit+1),
			}
			if stage != model.StagePublish {
				requests = append(requests, validAuthorize)
			}
			for _, request := range requests {
				conn := channelConnection(t, channel.listener.Addr().String())
				if _, err := io.WriteString(conn, request+"\n"); err != nil {
					t.Fatal(err)
				}
				requireChannelEOF(t, conn)
				conn.Close()
			}
			channel.close()
			if calls.Load() != 0 {
				t.Fatalf("rejected requests reached Control Plane: %d", calls.Load())
			}
			log, err := os.ReadFile(channel.log.Path())
			if err != nil || strings.Count(string(log), "\n") != len(requests) || strings.Contains(string(log), "contentId") {
				t.Fatalf("unsafe/missing rejection diagnostics: %q err=%v", log, err)
			}
		})
	}
}

func TestStageChannelLineLimitAndMultipleRequests(t *testing.T) {
	runner := &Runner{dataDir: t.TempDir()}
	channel, err := runner.openStageChannel(context.Background(), ulid.Make().String(), model.StageCheck, filepath.Join(channelDirectory(t), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer channel.close()
	conn := channelConnection(t, channel.listener.Addr().String())
	request := `{"op":"cancel_check"}`
	for _, line := range []string{request, request + strings.Repeat(" ", channelLineLimit-len(request)), request} {
		if got := channelReply(t, conn, line); got != `{"cancelRequested":false}` {
			t.Fatalf("reply=%s", got)
		}
	}
	// A second connection cannot turn a final unterminated JSON into a request.
	partial := channelConnection(t, channel.listener.Addr().String()).(*net.UnixConn)
	io.WriteString(partial, request)
	partial.CloseWrite()
	requireChannelEOF(t, partial)
}

func TestStageChannelCancelCheckUsesLatestHeartbeat(t *testing.T) {
	id := ulid.Make().String()
	var cancelRequested atomic.Bool
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		ids := []string{}
		if cancelRequested.Load() {
			ids = append(ids, id)
		}
		json.NewEncoder(w).Encode(map[string]any{"cancelExecutionIds": ids})
	}))
	defer server.Close()
	runner, err := New(testConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := runner.RegisterExecution(id, cancel); err != nil {
		t.Fatal(err)
	}
	channel, err := runner.openStageChannel(ctx, id, model.StagePrepare, filepath.Join(channelDirectory(t), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer channel.close()
	conn := channelConnection(t, channel.listener.Addr().String())
	if got := channelReply(t, conn, `{"op":"cancel_check"}`); got != `{"cancelRequested":false}` || calls.Load() != 0 {
		t.Fatalf("reply=%s HTTP=%d", got, calls.Load())
	}
	cancelRequested.Store(true)
	if err := runner.Heartbeat(context.Background()); err != nil || ctx.Err() == nil {
		t.Fatalf("heartbeat did not cancel execution: %v", err)
	}
	if got := channelReply(t, conn, `{"op":"cancel_check"}`); got != `{"cancelRequested":true}` || calls.Load() != 1 {
		t.Fatalf("reply=%s HTTP=%d", got, calls.Load())
	}
	runner.ForgetExecution(id)
	if got := channelReply(t, conn, `{"op":"cancel_check"}`); got != `{"cancelRequested":false}` {
		t.Fatalf("forgotten execution retained cancellation: %s", got)
	}
}

func TestStageChannelClosesIdlePartialAndInFlightConnections(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer server.Close()
	runner, err := New(testConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	channel, err := runner.openStageChannel(context.Background(), ulid.Make().String(), model.StagePublish, filepath.Join(channelDirectory(t), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer channel.close()
	idle := channelConnection(t, channel.listener.Addr().String())
	partial := channelConnection(t, channel.listener.Addr().String())
	io.WriteString(partial, `{"op":`)
	inFlight := channelConnection(t, channel.listener.Addr().String())
	io.WriteString(inFlight, "{\"op\":\"authorize\",\"operation\":\"publish\",\"approvalId\":null,\"contentId\":\"r1\"}\n")
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not start")
	}
	channel.close()
	for _, conn := range []net.Conn{idle, partial, inFlight} {
		requireChannelEOF(t, conn)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("closing the stage did not cancel its relay")
	}
}

func TestStageChannelLifetimeInFramework(t *testing.T) {
	runner, claim, _, services := stageFixture(t, true)
	var previous string
	services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
		if previous != "" {
			if conn, err := net.Dial("unix", previous); err == nil {
				conn.Close()
				t.Fatal("previous stage channel is still listening")
			}
		}
		if in.Stage == model.StageWork {
			if in.ChannelPath != "" {
				t.Fatal("work received a channel")
			}
		} else {
			previous = in.ChannelPath
			conn := channelConnection(t, in.ChannelPath)
			if got := channelReply(t, conn, `{"op":"cancel_check"}`); got != `{"cancelRequested":false}` {
				t.Fatalf("reply=%s", got)
			}
		}
		writeStageOutput(t, in)
		return successfulStage(in.Stage)
	})
	if err := runner.ExecuteStages(context.Background(), claim, services); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(previous); !os.IsNotExist(err) {
		t.Fatalf("last stage channel remains: %v", err)
	}
	for _, kind := range []model.Stage{model.StageWork, model.Stage("proposal")} {
		if c, err := runner.openStageChannel(context.Background(), claim.Execution.ID, kind, filepath.Join(channelDirectory(t), "s")); err == nil {
			c.close()
			t.Fatalf("%s received a channel", kind)
		}
	}
}

func TestStageChannelRejectionDeliveredAsRunnerLog(t *testing.T) {
	f := newLogControlPlane(t)
	runner, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	outcome := runner.runStageWithChannel(ctx, logExecution,
		StageInvocation{Stage: model.StageCheck, Directory: channelDirectory(t)},
		stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
			conn := channelConnection(t, in.ChannelPath)
			io.WriteString(conn, "{\"op\":\"authorize\",\"operation\":\"publish\",\"approvalId\":null,\"contentId\":\"private-content\"}\n")
			requireChannelEOF(t, conn)
			return successfulStage(in.Stage)
		}))
	if outcome.RunnerError != nil || ctx.Err() != nil {
		t.Fatalf("outcome=%+v delivery=%v", outcome, ctx.Err())
	}
	var payload string
	if err := f.db.QueryRow(`SELECT payload FROM execution_events WHERE execution_id=? AND type='log'`, logExecution).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event struct{ Stream, Text, SourceID string }
	if json.Unmarshal([]byte(payload), &event) != nil || event.Stream != "runner" || event.SourceID == "" ||
		!strings.Contains(event.Text, "forbidden") || strings.Contains(payload, "private-content") {
		t.Fatalf("wrong or unsafe rejection log: %s", payload)
	}
	var stage string
	if err := f.db.QueryRow(`SELECT stage FROM artifacts WHERE execution_id=? AND kind='log'`, logExecution).Scan(&stage); err != nil || stage != "runner" {
		t.Fatalf("runner log artifact stage=%q err=%v", stage, err)
	}
}

func TestStageChannelCreationFailureDoesNotRunStageOrOverwriteFile(t *testing.T) {
	runner := &Runner{dataDir: t.TempDir()}
	dir := channelDirectory(t)
	path := filepath.Join(dir, "channel.sock")
	if err := os.WriteFile(path, []byte("existing file"), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	outcome := runner.runStageWithChannel(context.Background(), ulid.Make().String(),
		StageInvocation{Stage: model.StagePrepare, Directory: dir},
		stageExecutorFunc(func(context.Context, StageInvocation) StageOutcome {
			called = true
			return StageOutcome{}
		}))
	data, err := os.ReadFile(path)
	if called || outcome.RunnerError == nil || *outcome.RunnerError != "sandbox_error" || err != nil || string(data) != "existing file" {
		t.Fatalf("called=%v outcome=%+v file=%q err=%v", called, outcome, data, err)
	}
}
