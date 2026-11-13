package claudecode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// Opt-in acceptance test: real Claude, real container runtime, real model API.
// No Launch/Process/output parser is substituted. Missing prerequisites fail
// once enabled; normal verification never borrows personal credentials.
func TestClaudeCodeLive(t *testing.T) {
	if os.Getenv("EVALOOM_CLAUDE_LIVE_TEST") != "1" {
		t.Skip("set EVALOOM_CLAUDE_LIVE_TEST=1 with dedicated authentication and installed Linux images")
	}
	required := func(key string) string {
		t.Helper()
		value := os.Getenv(key)
		if value == "" {
			t.Fatalf("%s is required", key)
		}
		return value
	}
	image := required("EVALOOM_CLAUDE_TEST_IMAGE")
	proxyImage := required("EVALOOM_CLAUDE_TEST_PROXY_IMAGE")
	proxyBinary := required("EVALOOM_CLAUDE_TEST_PROXY_BINARY")
	store := required("EVALOOM_CLAUDE_TEST_AUTH_FILE")
	model := required("EVALOOM_CLAUDE_TEST_MODEL")
	runtime, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"workspace_change", "cancel", "work_failed"} {
		t.Run(mode, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"workspace", "git", "configuration", "personal-home", "data"} {
				if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			// Canary B proves the CLI loaded Q142's dedicated instruction file.
			instructions := "For this fixture only: when asked to write change.txt, its exact content must be evaloom-adapter-live."
			sha := strings.Repeat("a", 40)
			configuration, err := harnessenv.Create(ctx, harnessenv.Config{Root: filepath.Join(root, "configuration"),
				CommitSHA: sha, ConfigVariable: "CLAUDE_CONFIG_DIR", Instructions: []string{"CLAUDE.md"},
				Files: []gitcontext.File{{Path: "CLAUDE.md", Content: instructions,
					ContentHash: fmt.Sprintf("%x", sha256.Sum256([]byte(instructions))), SourceCommitSHA: sha}}})
			if err != nil {
				t.Fatal(err)
			}
			defer configuration.Close()
			config := sandbox.ContainerConfig{RuntimePath: runtime, Image: image, ProxyImage: proxyImage, ProxyBinary: proxyBinary,
				RuntimeEnv: map[string]string{}, AllowEnv: []string{"CLAUDE_CONFIG_DIR"}, ControlTimeout: 10 * time.Second,
				WorktreeRoot: filepath.Join(root, "workspace"), GitDirRoot: filepath.Join(root, "git"), ConfigDirRoot: filepath.Join(root, "configuration"),
				HostHome: filepath.Join(root, "personal-home"), DataDir: filepath.Join(root, "data"), RunnerTokenFile: filepath.Join(root, "data", "token")}
			for _, key := range []string{"HOME", "PATH", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
				if value, ok := os.LookupEnv(key); ok {
					config.RuntimeEnv[key] = value
				}
			}
			h := New(Config{Launch: ContainerLaunch(config, func(env adapter.SandboxEnv) (sandbox.SandboxSpec, error) {
				return sandbox.SandboxSpec{Kind: sandbox.Harness, Worktree: env.Worktree, GitDir: config.GitDirRoot, ConfigDir: env.ConfigDir,
					Env: env.Env, Network: sandbox.NetworkHarness,
					Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 2 << 30, Time: 2 * time.Minute}}, nil
			}), ResolveConfig: func(adapter.SandboxEnv) (*harnessenv.Environment, error) { return configuration, nil },
				ResolveAuthentication: func(adapter.HarnessRequest) (string, error) { return store, nil },
				ResolveToolPermissions: func(context.Context, adapter.HarnessRequest) (json.RawMessage, error) {
					return json.RawMessage(`{"allow":["Write","Bash"],"deny":[]}`), nil
				}, CancelGrace: sandbox.TerminationGrace})
			prompt := "Write change.txt using the exact content specified in CLAUDE.md. Leave the file in the working directory."
			if mode == "cancel" {
				prompt = "Run Bash command: printf started > started; sleep 600; printf unexpected > finished. Run it in the foreground and wait for it."
			}
			req := adapter.HarnessRequest{ContractVersion: adapter.ContractVersion, ExecutionID: "live-test",
				Task:    adapter.RequestTask{Title: prompt, AcceptanceCriteria: "file remains in workspace"},
				Project: adapter.RequestProject{Workdir: config.WorktreeRoot},
				Profile: adapter.RequestProfile{ModelID: model, Definition: json.RawMessage(`{}`)},
				Context: adapter.RequestContext{Instructions: "Perform the fixture task. Do not commit.", RevisionIDs: []string{}}}
			if mode == "work_failed" {
				req.Profile.ModelID = "evaloom-intentionally-nonexistent-model"
			}
			id, err := h.Start(ctx, req, adapter.SandboxEnv{Worktree: config.WorktreeRoot, ConfigDir: configuration.ConfigDir(), Env: configuration.Env()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.Cancel(id); _, _ = h.Result(id) })
			if mode == "cancel" {
				ticker := time.NewTicker(20 * time.Millisecond)
				defer ticker.Stop()
				for {
					if _, err := os.Stat(filepath.Join(config.WorktreeRoot, "started")); err == nil {
						break
					}
					if status, err := h.Status(id); err != nil || status != adapter.SessionRunning {
						t.Fatal("Claude stopped before starting the foreground command")
					}
					select {
					case <-ctx.Done():
						t.Fatal("Claude did not start the foreground command before the test deadline")
					case <-ticker.C:
					}
				}
				if err := h.Cancel(id); err != nil {
					t.Fatal(err)
				}
			}
			result, err := h.Result(id)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "work_failed" && (result.SessionID == "" || result.SessionID == id) {
				t.Fatal("CLI native session ID missing")
			}
			if mode == "workspace_change" {
				if result.Status != adapter.ResultSucceeded || result.ErrorClass != nil {
					t.Fatalf("live execution failed: status=%s errorClass=%v", result.Status, result.ErrorClass)
				}
				content, err := os.ReadFile(filepath.Join(config.WorktreeRoot, "change.txt"))
				if err != nil || strings.TrimSpace(string(content)) != "evaloom-adapter-live" {
					t.Fatal("workspace change or dedicated instructions missing", err)
				}
			} else {
				want := adapter.ErrorExecutionFailed
				if mode == "cancel" {
					want = adapter.ErrorCancelled
				}
				if result.Status != adapter.ResultFailed || result.ErrorClass == nil || *result.ErrorClass != want {
					t.Fatalf("want failed/%s, got status=%s errorClass=%v", want, result.Status, result.ErrorClass)
				}
				if _, err := os.Stat(filepath.Join(config.WorktreeRoot, "finished")); !os.IsNotExist(err) {
					t.Fatal("cancelled command reached its final write")
				}
			}
			if _, err := os.Stat(config.WorktreeRoot); err != nil {
				t.Fatal("workspace not retained", err)
			}
		})
	}
}
