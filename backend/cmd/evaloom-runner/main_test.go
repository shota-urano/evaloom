package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
		if r.Method == "POST" && r.URL.Path == "/api/runners/mac-mini/claim" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
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
	output := make(startupOutput, 8)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", configPath}, output) }()
	select {
	case <-request:
	case <-time.After(time.Second):
		t.Fatal("runner did not send heartbeat immediately on startup")
	}
	select {
	case line := <-output:
		if strings.Contains(line, "not implemented") {
			t.Fatalf("obsolete startup message: %s", line)
		}
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
	config := runner.Config{
		RunnerID: "mac-mini", ControlPlaneURL: endpoint, TokenFile: tokenFile,
		WorkspaceRoot: filepath.Join(dir, "worktrees"), DataDir: filepath.Join(dir, "data"),
		SecretsDir:            filepath.Join(dir, "data", "secrets"),
		MicroHarnessAuthFiles: map[string]string{"codex": filepath.Join(dir, "data", "auth.json")},
		Container:             runner.ContainerConfig{RuntimePath: "/usr/local/bin/docker", Image: "harness:test", ProxyImage: "proxy:test", ProxyBinary: "/opt/evaloom/proxy", CPUs: 4, MemoryMiB: 8192},
	}
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
	output := make(startupOutput, 8)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", path}, output) }()
	deadline := time.After(time.Second)
	for failed := false; !failed; {
		select {
		case line := <-output:
			failed = strings.Contains(line, "HTTP 503")
		case <-deadline:
			t.Fatal("runner did not report failed heartbeat")
		}
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

func TestStartupRejectsInvalidConfiguration(t *testing.T) {
	// Re-execute the test binary through the real main, including os.Exit(1).
	if path := os.Getenv("EVALOOM_TEST_STARTUP_CONFIG"); path != "" {
		os.Args = []string{"evaloom-runner", "-config", path}
		main()
		return
	}
	basePath := writeConfig(t, "https://control.example.test")
	base, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	type invalidConfig struct {
		name string
		edit func(map[string]any)
	}
	var cases []invalidConfig
	for _, key := range []string{"container.cpus", "container.memoryMiB"} {
		for _, value := range []int{0, -1} {
			cases = append(cases, invalidConfig{"nonpositive/" + key + "/" + strconv.Itoa(value), func(config map[string]any) {
				object, field := configObject(config, key)
				object[field] = value
			}})
		}
	}
	for _, key := range []string{
		"runnerId", "controlPlaneUrl", "tokenFile", "workspaceRoot", "dataDir", "secretsDir", "microHarnessAuthFiles",
		"container", "container.runtimePath", "container.image", "container.proxyImage", "container.proxyBinary",
	} {
		cases = append(cases, invalidConfig{"missing/" + key, func(config map[string]any) {
			object, field := configObject(config, key)
			delete(object, field)
		}})
	}
	for _, key := range []string{"worktreeRoot", "harnessAuthFiles", "defaultTimeouts"} {
		cases = append(cases, invalidConfig{"removed/" + key, func(config map[string]any) { config[key] = map[string]any{} }})
	}
	for _, kind := range []string{"equal", "workspace inside data", "data inside workspace"} {
		cases = append(cases, invalidConfig{"overlap/" + kind, func(config map[string]any) {
			switch kind {
			case "equal":
				config["workspaceRoot"] = config["dataDir"]
			case "workspace inside data":
				config["workspaceRoot"] = config["dataDir"].(string) + "/workspaces"
			case "data inside workspace":
				config["workspaceRoot"] = filepath.Dir(config["dataDir"].(string))
			}
		}})
	}
	for _, section := range []string{"", "container"} {
		cases = append(cases, invalidConfig{"unknown/" + section, func(config map[string]any) {
			object := config
			if section != "" {
				object = config[section].(map[string]any)
			}
			object["secret-unknown-key"] = "secret-value"
		}})
	}
	for _, key := range []string{"tokenFile", "workspaceRoot", "dataDir", "secretsDir", "container.runtimePath", "container.proxyBinary", "microHarnessAuthFiles.codex"} {
		cases = append(cases, invalidConfig{"relative/" + key, func(config map[string]any) {
			object, field := configObject(config, key)
			object[field] = "secret-relative-path"
		}})
	}
	cases = append(cases,
		invalidConfig{"auth outside dataDir", func(config map[string]any) {
			config["microHarnessAuthFiles"] = map[string]any{"codex": "/secret-outside/auth.json"}
		}},
		invalidConfig{"case alias", func(config map[string]any) { config["RunnerId"] = config["runnerId"]; delete(config, "runnerId") }},
	)
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var config map[string]any
			if err := json.Unmarshal(base, &config); err != nil {
				t.Fatal(err)
			}
			tt.edit(config)
			content, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "runner.json")
			if err := os.WriteFile(path, content, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStartupRejectsInvalidConfiguration$")
			cmd.Env = append(os.Environ(), "EVALOOM_TEST_STARTUP_CONFIG="+path)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("runner did not exit with startup error: %v\n%s", err, output)
			}
			if !bytes.Contains(output, []byte("runner:")) || bytes.Contains(output, []byte("started")) {
				t.Fatalf("unexpected startup output: %s", output)
			}
			for _, privateValue := range []string{"secret-value", "secret-unknown-key", "secret-relative-path", "secret-outside"} {
				if bytes.Contains(output, []byte(privateValue)) {
					t.Fatalf("configuration value echoed in startup output: %s", output)
				}
			}
		})
	}
}

func configObject(config map[string]any, key string) (map[string]any, string) {
	parts := strings.Split(key, ".")
	if len(parts) == 1 {
		return config, key
	}
	return config[parts[0]].(map[string]any), parts[1]
}
