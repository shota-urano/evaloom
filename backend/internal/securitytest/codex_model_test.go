//go:build security_model

package securitytest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// This acceptance test uses the real CLI and product ContainerLaunch. Neither
// model output nor process supervision is replaced. Run on the dedicated model
// test machine using the same environment as TestModelConfigurationIsolation.
func TestCodexWorkspaceChangeAndCancellation(t *testing.T) {
	required := func(name string) string {
		t.Helper()
		value := os.Getenv(name)
		if strings.TrimSpace(value) == "" {
			t.Fatalf("実行不能: %s is required; see securitytest/README.md §3.1", name)
		}
		return value
	}
	image := required("EVALOOM_SECURITY_MODEL_IMAGE")
	proxyImage := required("EVALOOM_SECURITY_MODEL_PROXY_IMAGE")
	proxyBinary := required("EVALOOM_SECURITY_MODEL_PROXY_BINARY")
	modelID := required("EVALOOM_SECURITY_CODEX_MODEL")
	auth := required("EVALOOM_SECURITY_CODEX_AUTH_JSON")
	runtime, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = filepath.Abs(runtime)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewFixture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	cli := configurationCLIs[0] // codex; same pinned configuration/auth plumbing.
	profile, files := configurationProfile(t, ctx, f, cli, modelID, NewCanary())
	credential, err := configurationCredential(cli, profile.CommitSHA, auth)
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(root, "model-auth.json")
	configurationWrite(t, store, credential.Content)
	settings := configurationEnvironment(t, ctx, filepath.Join(root, "configuration"), cli, profile, files)
	vars := settings.Env()
	vars["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	vars["GIT_CONFIG_NOSYSTEM"], vars["GIT_TERMINAL_PROMPT"] = "1", "0"
	vars["GIT_CONFIG_GLOBAL"] = filepath.Join(settings.ConfigDir(), ".gitconfig")
	runtimeEnv := map[string]string{"HOME": f.Home, "PATH": os.Getenv("PATH"), "DOCKER_CONFIG": filepath.Join(f.Home, ".docker")}
	if value := os.Getenv("DOCKER_HOST"); value != "" {
		runtimeEnv["DOCKER_HOST"] = value
	}
	config := sandbox.ContainerConfig{
		RuntimePath: runtime, Image: image, ProxyImage: proxyImage, ProxyBinary: proxyBinary, RuntimeEnv: runtimeEnv,
		AllowEnv:     []string{"CODEX_HOME", "PATH", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_TERMINAL_PROMPT"},
		WorktreeRoot: f.Worktree, GitDirRoot: filepath.Join(f.Repository, ".git"), ConfigDirRoot: filepath.Dir(settings.ConfigDir()),
		HostHome: f.Home, DataDir: filepath.Join(root, "data"), RunnerTokenFile: filepath.Join(root, "runner-token"), ControlTimeout: 15 * time.Second,
	}
	launch := codex.ContainerLaunch(config, func(env adapter.SandboxEnv) (sandbox.SandboxSpec, error) {
		return sandbox.SandboxSpec{Kind: sandbox.Harness, Worktree: env.Worktree, GitDir: config.GitDirRoot,
			ConfigDir: env.ConfigDir, Env: env.Env, Network: sandbox.NetworkHarness,
			Limits: sandbox.ResourceLimits{CPU: 2, MemoryBytes: 2 << 30, Time: 300 * time.Second}}, nil
	})
	h := codex.New(codex.Config{Launch: launch, CancelGrace: sandbox.TerminationGrace,
		ResolveConfig:         func(adapter.SandboxEnv) (*harnessenv.Environment, error) { return settings, nil },
		ResolveAuthentication: func(adapter.HarnessRequest) (string, error) { return store, nil },
		ResolveToolPermissions: func(context.Context, adapter.HarnessRequest) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	})
	env := adapter.SandboxEnv{Worktree: f.Worktree, ConfigDir: settings.ConfigDir(), Env: vars}
	start := func(instructions string) adapter.SessionID {
		t.Helper()
		req := configurationRequest(f, profile)
		req.Context.Instructions = instructions
		req.Task.Title, req.Task.Body, req.Task.AcceptanceCriteria = "Adapter acceptance", "", "Leave the requested workspace file"
		id, err := h.Start(ctx, req, env)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Cancel(id); _, _ = h.Result(id) })
		return id
	}
	id := start("Create adapter-change.txt containing exactly 'Codex change' followed by a newline, then finish. Do not commit or publish anything.")
	result, err := h.Result(id)
	// Do not print raw logs, which can contain authentication data.
	if err != nil || result.Status != adapter.ResultSucceeded || result.ErrorClass != nil || result.SessionID == "" || result.SessionID == id {
		t.Fatalf("real CLI failed: status=%s errorClass=%v err=%v", result.Status, result.ErrorClass, err)
	}
	data, err := os.ReadFile(filepath.Join(f.Worktree, "adapter-change.txt"))
	if err != nil || string(data) != "Codex change\n" {
		t.Fatalf("real CLI workspace change missing: %v", err)
	}
	head, err := f.Git(ctx, "rev-parse", "HEAD")
	if err != nil || head != f.BaseSHA {
		t.Fatalf("work stage unexpectedly committed: %v", err)
	}
	id = start("Run one shell command that writes adapter-running.txt containing 'running', then sleeps for 600 seconds. Wait for that command. Do not commit or publish anything.")
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(f.Worktree, "adapter-running.txt")); err == nil {
			break
		}
		status, err := h.Status(id)
		if err != nil || status != adapter.SessionRunning {
			t.Fatalf("CLI stopped before cancellation marker: status=%s err=%v", status, err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("real CLI did not reach cancellation marker")
		case <-ticker.C:
		}
	}
	if err := h.Cancel(id); err != nil {
		t.Fatal(err)
	}
	result, err = h.Result(id) // Product Wait confirms every Sandbox writer stopped.
	if err != nil || result.Status != adapter.ResultFailed || result.ErrorClass == nil || string(*result.ErrorClass) != "cancelled" {
		t.Fatalf("real CLI cancellation: status=%s errorClass=%v err=%v", result.Status, result.ErrorClass, err)
	}
	if _, err := os.Stat(filepath.Join(f.Worktree, "adapter-change.txt")); err != nil {
		t.Fatalf("workspace not retained after cancellation: %v", err)
	}
}
