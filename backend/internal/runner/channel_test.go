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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/sandboxproxy"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

func channelDirectory(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(t.TempDir(), "channel-")
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

// testChannelSocket serves handler the way a stage reaches it: through the
// production relay sidecar code (sandboxproxy.ServeChannel) on a socket, and
// the production Runner side of the sidecar's stdio (sandbox.RelayChannel).
// Only the container runtime is absent.
func testChannelSocket(t *testing.T, handler sandbox.ChannelHandler) string {
	t.Helper()
	if handler == nil {
		t.Fatal("stage has no channel")
	}
	dir, err := os.MkdirTemp("", "ch-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "c.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	served, relayed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(served)
		_ = sandboxproxy.ServeChannel(listener, stdinReader, stdoutWriter)
		stdoutWriter.Close()
	}()
	go func() {
		defer close(relayed)
		sandbox.RelayChannel(stdoutReader, stdinWriter, handler)
		stdinWriter.Close()
	}()
	t.Cleanup(func() {
		// Removing the sidecar: its stdio and every stage connection close.
		stdinWriter.Close()
		stdoutReader.Close()
		listener.Close()
		<-served
		<-relayed
		_ = os.RemoveAll(dir)
	})
	return path
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
			outcome := runner.runStageWithChannel(context.Background(), id,
				StageInvocation{Stage: model.StagePublish, Directory: channelDirectory(t)},
				stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
					conn = channelConnection(t, testChannelSocket(t, in.Channel))
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
			// A finished stage answers nothing more, even on an open connection.
			io.WriteString(conn, `{"op":"cancel_check"}`+"\n")
			requireChannelEOF(t, conn)
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
			channel, err := runner.openStageChannel(context.Background(), ulid.Make().String(), stage)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(channel.close)
			socket := testChannelSocket(t, channel)
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
				conn := channelConnection(t, socket)
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
			// The relay sidecar drops lines that are not one JSON value within
			// the limit; every other rejection is the Runner's and is logged.
			relayed := 0
			for _, request := range requests {
				if json.Valid([]byte(request)) && len(request) <= channelLineLimit {
					relayed++
				}
			}
			log, err := os.ReadFile(channel.log.Path())
			if err != nil || relayed != len(requests)-2 || strings.Count(string(log), "\n") != relayed || strings.Contains(string(log), "contentId") {
				t.Fatalf("unsafe/missing rejection diagnostics: %q err=%v", log, err)
			}
		})
	}
}

func TestStageChannelLineLimitAndMultipleRequests(t *testing.T) {
	runner := &Runner{dataDir: t.TempDir()}
	channel, err := runner.openStageChannel(context.Background(), ulid.Make().String(), model.StageCheck)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.close()
	socket := testChannelSocket(t, channel)
	conn := channelConnection(t, socket)
	request := `{"op":"cancel_check"}`
	for _, line := range []string{request, request + strings.Repeat(" ", channelLineLimit-len(request)), request} {
		if got := channelReply(t, conn, line); got != `{"cancelRequested":false}` {
			t.Fatalf("reply=%s", got)
		}
	}
	// A second connection cannot turn a final unterminated JSON into a request.
	partial := channelConnection(t, socket).(*net.UnixConn)
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
	channel, err := runner.openStageChannel(ctx, id, model.StagePrepare)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.close()
	conn := channelConnection(t, testChannelSocket(t, channel))
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

// Closing the stage's channel cancels an in-flight authorize and answers
// nothing more; the sidecar's removal then closes the stage's connections.
func TestStageChannelCloseCancelsInFlightRequest(t *testing.T) {
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
	channel, err := runner.openStageChannel(context.Background(), ulid.Make().String(), model.StagePublish)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.close()
	socket := testChannelSocket(t, channel)
	idle := channelConnection(t, socket)
	inFlight := channelConnection(t, socket)
	io.WriteString(inFlight, "{\"op\":\"authorize\",\"operation\":\"publish\",\"approvalId\":null,\"contentId\":\"r1\"}\n")
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not start")
	}
	channel.close()
	requireChannelEOF(t, inFlight)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("closing the stage did not cancel its relay")
	}
	io.WriteString(idle, `{"op":"cancel_check"}`+"\n")
	requireChannelEOF(t, idle)
}

func TestStageChannelLifetimeInFramework(t *testing.T) {
	runner, claim, _, services := stageFixture(t, true)
	var previous sandbox.ChannelHandler
	services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
		if previous != nil && previous.HandleChannelRequest([]byte(`{"op":"cancel_check"}`)) != nil {
			t.Fatal("previous stage channel still answers")
		}
		if in.Stage == model.StageWork {
			if in.Channel != nil {
				t.Fatal("work received a channel")
			}
		} else {
			previous = in.Channel
			conn := channelConnection(t, testChannelSocket(t, in.Channel))
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
	if previous == nil || previous.HandleChannelRequest([]byte(`{"op":"cancel_check"}`)) != nil {
		t.Fatal("last stage channel still answers")
	}
	for _, kind := range []model.Stage{model.StageWork, model.Stage("proposal")} {
		if c, err := runner.openStageChannel(context.Background(), claim.Execution.ID, kind); err == nil {
			c.close()
			t.Fatalf("%s received a channel", kind)
		}
	}
}

func TestExecuteStagesLongDataDirChannelRelay(t *testing.T) {
	runner, claim, reporter, services := stageFixture(t, true)
	// No host socket exists any more (spec 09 3.6, Q232), so a dataDir beyond
	// the sockaddr_un limits, with spaces as in ~/Library/Application Support,
	// changes nothing. Keep the specified layout.
	runner.dataDir = filepath.Join(runner.dataDir, "Application Support", strings.Repeat("runner-data-", 12))
	if err := secretstore.New(runner.dataDir).Set(claim.Profile.HarnessID, "delivery", "write", []byte("delivery-private-value")); err != nil {
		t.Fatal(err)
	}
	id, approval := claim.Execution.ID, ulid.Make().String()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Rejected prepare/check requests produce Runner log reports, not
		// authorization calls. Acknowledge those through their existing APIs.
		if r.URL.Path == "/api/executions/"+id+"/events" || r.URL.Path == "/api/executions/"+id+"/artifacts" {
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		calls.Add(1)
		if r.URL.Path != "/api/executions/"+id+"/authorize" || r.Method != http.MethodPost ||
			r.Header.Get("Authorization") != "Bearer "+runner.token {
			t.Error("incorrect relay path, method or Runner credentials")
		}
		var input contract.AuthorizeRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Operation != "merge_pull_request" ||
			input.ContentID != "opaque:revision-1" || input.ApprovalID == nil || *input.ApprovalID != approval {
			t.Errorf("incorrect relay payload: %+v err=%v", input, err)
		}
		io.WriteString(w, `{"allowed":true}`)
	}))
	defer server.Close()
	runner.controlPlaneURL, runner.client = server.URL, server.Client()
	var stages []model.Stage
	var connections []net.Conn
	services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
		stages = append(stages, in.Stage)
		if in.Stage == model.StageWork {
			if in.Channel != nil {
				t.Fatal("work received a channel")
			}
		} else {
			if entries, err := os.ReadDir(in.Directory); err != nil {
				t.Fatal(err)
			} else {
				for _, entry := range entries {
					if entry.Type()&os.ModeSocket != 0 {
						t.Fatalf("host socket in the attempt directory: %s", entry.Name())
					}
				}
			}
			conn := channelConnection(t, testChannelSocket(t, in.Channel))
			connections = append(connections, conn)
			if got := channelReply(t, conn, `{"op":"cancel_check"}`); got != `{"cancelRequested":false}` {
				t.Fatalf("cancel_check reply=%s", got)
			}
			request := `{"op":"authorize","operation":"merge_pull_request","approvalId":"` + approval + `","contentId":"opaque:revision-1"}`
			if in.Stage == model.StagePublish {
				if got := channelReply(t, conn, request); got != `{"allowed":true}` {
					t.Fatalf("authorize reply=%s", got)
				}
			} else {
				if _, err := io.WriteString(conn, request+"\n"); err != nil {
					t.Fatal(err)
				}
				requireChannelEOF(t, conn)
				if calls.Load() != 0 {
					t.Fatalf("%s authorize reached Control Plane", in.Stage)
				}
			}
		}
		writeStageOutput(t, in)
		return successfulStage(in.Stage)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := runner.ExecuteStages(ctx, claim, services); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stages, executionStageOrder) || calls.Load() != 1 {
		t.Fatalf("stages=%v relay calls=%d outcomes=%+v", stages, calls.Load(), reporter.outcomes)
	}
	for _, outcome := range reporter.outcomes {
		if outcome.RunnerError != nil {
			t.Fatalf("stage failed: %+v", outcome)
		}
	}
	for _, conn := range connections {
		io.WriteString(conn, `{"op":"cancel_check"}`+"\n")
		requireChannelEOF(t, conn)
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
			conn := channelConnection(t, testChannelSocket(t, in.Channel))
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

func TestStageChannelCreationFailureDoesNotRunStage(t *testing.T) {
	// The channel's runner log cannot be created: dataDir is a file.
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(dataDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{dataDir: dataDir}
	called := false
	outcome := runner.runStageWithChannel(context.Background(), ulid.Make().String(),
		StageInvocation{Stage: model.StagePrepare, Directory: channelDirectory(t)},
		stageExecutorFunc(func(context.Context, StageInvocation) StageOutcome {
			called = true
			return StageOutcome{}
		}))
	if called || outcome.RunnerError == nil || *outcome.RunnerError != "sandbox_error" {
		t.Fatalf("called=%v outcome=%+v", called, outcome)
	}
}
