package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/runner"
)

type startupOutput chan string

func (output startupOutput) Write(p []byte) (int, error) {
	output <- string(p)
	return len(p), nil
}

func TestRunWaitsUntilShutdown(t *testing.T) {
	request := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/runners/mac-mini/heartbeat" {
			t.Errorf("unexpected startup request: %s %s", r.Method, r.URL.Path)
		}
		var input struct {
			BootID string   `json:"bootId"`
			IDs    []string `json:"runningExecutionIds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.BootID == "" || input.IDs == nil || len(input.IDs) != 0 {
			t.Errorf("invalid initial heartbeat: %+v %v", input, err)
		}
		request <- input.BootID
		io.WriteString(w, `{"cancelExecutionIds":[]}`)
	}))
	defer server.Close()
	configPath := writeConfig(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := make(startupOutput, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", configPath}, output) }()
	select {
	case <-request:
	case <-time.After(time.Second):
		t.Fatal("runner did not send heartbeat immediately on startup")
	}
	select {
	case <-output:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("runner exited before shutdown: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not stop after cancellation")
	}
}

func TestRunRejectsUnexpectedArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"unexpected"}, {"-db", "control-plane.db"}, {"-config", "missing.json"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Fatalf("run(%v) succeeded; want usage error", args)
		}
	}
}

func writeConfig(t *testing.T, endpoint string) string {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "runner.token")
	if err := os.WriteFile(tokenFile, []byte(base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0600); err != nil {
		t.Fatal(err)
	}
	config := runner.Config{RunnerID: "mac-mini", ControlPlaneURL: endpoint, TokenFile: tokenFile, WorktreeRoot: filepath.Join(dir, "worktrees"), DataDir: filepath.Join(dir, "data")}
	content, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runner.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunSurvivesControlPlaneFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	path := writeConfig(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := make(startupOutput, 2)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", path}, output) }()
	select {
	case line := <-output:
		if !strings.Contains(line, "HTTP 503") {
			t.Fatalf("heartbeat error not reported: %s", line)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not report failed heartbeat")
	}
	select {
	case err := <-done:
		t.Fatalf("runner exited on heartbeat failure: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not stop")
	}
}

func TestRunHelpExitsWithoutStarting(t *testing.T) {
	if err := run(context.Background(), []string{"-help"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}
