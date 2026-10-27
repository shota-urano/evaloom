package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(t *testing.T, endpoint string) Config {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "runner.token")
	if err := os.WriteFile(tokenFile, []byte(testToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return Config{"mac-mini", endpoint, tokenFile, filepath.Join(dir, "worktrees"), filepath.Join(dir, "data")}
}

var testToken = base64.RawURLEncoding.EncodeToString(make([]byte, 32))

type heartbeatInput struct {
	BootID              string   `json:"bootId"`
	RunningExecutionIDs []string `json:"runningExecutionIds"`
}

func readHeartbeat(t *testing.T, r *http.Request) heartbeatInput {
	t.Helper()
	if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Origin") != "" || len(r.Cookies()) != 0 {
		t.Error("unexpected heartbeat method or authentication headers")
	}
	var input heartbeatInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		t.Error(err)
	}
	if input.BootID == "" || input.RunningExecutionIDs == nil {
		t.Errorf("heartbeat must include bootId and a non-null ID array: %+v", input)
	}
	return input
}

func TestHeartbeatConfiguredURLAndBootIdentity(t *testing.T) {
	// Changing only the configuration routes requests to a different server.
	var boots []string
	for range 2 {
		received := make(chan heartbeatInput, 2)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/control/api/runners/mac-mini/heartbeat" {
				t.Errorf("unexpected path %q", r.URL.Path)
			}
			received <- readHeartbeat(t, r)
			io.WriteString(w, `{"cancelExecutionIds":[]}`)
		}))
		defer server.Close()
		process, err := New(testConfig(t, server.URL+"/control/"))
		if err != nil {
			t.Fatal(err)
		}
		var inputs []heartbeatInput
		for range 2 {
			if err := process.Heartbeat(context.Background()); err != nil {
				t.Fatal(err)
			}
			inputs = append(inputs, <-received)
		}
		if len(inputs) != 2 || inputs[0].BootID != inputs[1].BootID || len(inputs[0].RunningExecutionIDs) != 0 {
			t.Fatalf("bootId changed within a process or initial IDs were not empty: %+v", inputs)
		}
		boots = append(boots, inputs[0].BootID)
	}
	if boots[0] == boots[1] {
		t.Fatal("new Runner reused previous bootId")
	}
}

func TestHeartbeatCancelsOnlyRequestedRunningExecutions(t *testing.T) {
	const first = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	const second = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	received := make(chan heartbeatInput, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- readHeartbeat(t, r)
		io.WriteString(w, `{"cancelExecutionIds":["`+first+`","already-finished"]}`)
	}))
	defer server.Close()
	process, err := New(testConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	for id, cancel := range map[string]context.CancelFunc{first: cancelFirst, second: cancelSecond} {
		if err := process.RegisterExecution(id, cancel); err != nil {
			t.Fatal(err)
		}
	}
	var inputs []heartbeatInput
	for range 2 { // Repeated delivery remains harmless and IDs stay reported.
		if err := process.Heartbeat(context.Background()); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, <-received)
		if firstCtx.Err() != context.Canceled || secondCtx.Err() != nil {
			t.Fatal("heartbeat did not cancel exactly the requested execution")
		}
	}
	process.ForgetExecution(first)
	if err := process.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	inputs = append(inputs, <-received)
	if !reflect.DeepEqual(inputs[0].RunningExecutionIDs, []string{first, second}) || !reflect.DeepEqual(inputs[1].RunningExecutionIDs, []string{first, second}) || !reflect.DeepEqual(inputs[2].RunningExecutionIDs, []string{second}) {
		t.Fatalf("incorrect active IDs: %+v", inputs)
	}
}

func TestHeartbeatFailuresLeaveExecutionRunningAndCanResend(t *testing.T) {
	for _, reply := range []struct {
		name   string
		status int
		body   string
	}{
		{"unavailable", 503, testToken},
		{"unauthorized", 401, testToken},
		{"malformed", 200, testToken},
		{"missing array", 200, `{}`},
		{"null array", 200, `{"cancelExecutionIds":null}`},
		{"partial", 200, `{"cancelExecutionIds":["active"],`},
		{"trailing JSON", 200, `{"cancelExecutionIds":["active"]} {}`},
		{"oversized", 200, `{"cancelExecutionIds":["active"]}` + strings.Repeat(" ", 1<<20)},
	} {
		t.Run(reply.name, func(t *testing.T) {
			received := make(chan string, 2)
			var failed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- readHeartbeat(t, r).BootID
				if !failed.Swap(true) {
					w.WriteHeader(reply.status)
					io.WriteString(w, reply.body)
					return
				}
				io.WriteString(w, `{"cancelExecutionIds":["active"]}`)
			}))
			defer server.Close()
			process, err := New(testConfig(t, server.URL))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := process.RegisterExecution("active", cancel); err != nil {
				t.Fatal(err)
			}
			err = process.Heartbeat(context.Background())
			boot := <-received
			if err == nil || strings.Contains(err.Error(), testToken) || ctx.Err() != nil {
				t.Fatalf("failed heartbeat cancelled work or exposed token: %v", err)
			}
			if err := process.Heartbeat(context.Background()); err != nil || ctx.Err() != context.Canceled || boot != <-received {
				t.Fatalf("resend did not preserve bootId and process cancellation: %v", err)
			}
		})
	}
}

func TestHeartbeatDoesNotFollowRedirect(t *testing.T) {
	var received atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received.Store(true) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	process, err := New(testConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Heartbeat(context.Background()); err == nil || received.Load() {
		t.Fatal("redirect was followed or accepted")
	}
}

func TestHeartbeatTransportFailureAndShutdown(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	process, err := New(testConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- process.Heartbeat(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not stop on shutdown")
	}
	server.Close()
	if err := process.Heartbeat(context.Background()); err == nil {
		t.Fatal("unreachable Control Plane accepted")
	}
}

func TestCancellationMayFinishExecution(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"cancelExecutionIds":["active"]}`)
	}))
	defer server.Close()
	process, err := New(testConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := process.RegisterExecution("active", func() { process.ForgetExecution("active") }); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Heartbeat(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation deadlocked while finishing execution")
	}
}
