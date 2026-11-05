//go:build security_model

package securitytest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/claudecode"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/runner"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// This test must use the product environment, adapters AND ContainerLaunch.
// No fake CLI, probe adapter or host-execution fallback can satisfy spec 14 §3.1.
func TestModelConfigurationIsolation(t *testing.T) {
	for _, cli := range configurationCLIs {
		t.Run(cli.name, func(t *testing.T) { modelConfigurationIsolation(t, cli) })
	}
}

func modelConfigurationIsolation(t *testing.T, cli configurationCLI) {
	t.Helper()
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
	modelID, auth := required(cli.prefix+"MODEL"), required(cli.prefix+"AUTH_JSON")
	destinations := strings.Split(required(cli.prefix+"DESTINATIONS"), ",")
	for i := range destinations {
		destinations[i] = strings.TrimSpace(destinations[i])
	}
	runtime, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("実行不能: Docker CLI: %v", err)
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
		t.Fatalf("実行不能: %v", err)
	}
	a, b := NewCanary(), NewCanary()
	personalInstructions(t, f.Home, a)
	// Poison the ambient sources too. Container must not inherit any of them.
	t.Setenv("HOME", f.Home)
	t.Setenv("CODEX_HOME", filepath.Join(f.Home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(f.Home, ".claude"))
	profile, files := configurationProfile(t, ctx, f, cli, modelID, b)
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
	vars["PYTHONDONTWRITEBYTECODE"] = "1"
	// The runtime also gets a disposable HOME/config directory. In particular,
	// never inherit DOCKER_CONFIG or read ~/.docker in the operator's real HOME.
	runtimeEnv := map[string]string{"HOME": f.Home, "PATH": os.Getenv("PATH"), "DOCKER_CONFIG": filepath.Join(f.Home, ".docker")}
	if value := os.Getenv("DOCKER_HOST"); value != "" {
		runtimeEnv["DOCKER_HOST"] = value
	}
	config := sandbox.ContainerConfig{
		RuntimePath: runtime, Image: image, ProxyImage: proxyImage, ProxyBinary: proxyBinary, RuntimeEnv: runtimeEnv,
		AllowEnv:     []string{cli.variable, "PATH", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_TERMINAL_PROMPT", "PYTHONDONTWRITEBYTECODE"},
		WorktreeRoot: f.Worktree, GitDirRoot: filepath.Join(f.Repository, ".git"), ConfigDirRoot: filepath.Dir(settings.ConfigDir()),
		HostHome: f.Home, DataDir: filepath.Join(root, "data"), RunnerTokenFile: filepath.Join(root, "runner-token"), ControlTimeout: 15 * time.Second,
	}
	spec := sandbox.SandboxSpec{Kind: sandbox.Harness, Worktree: f.Worktree, GitDir: config.GitDirRoot, ConfigDir: settings.ConfigDir(), Env: vars,
		Network: sandbox.NetworkHarness, AllowedDestinations: destinations,
		Limits: sandbox.ResourceLimits{CPU: 2, MemoryBytes: 2 * 1024 * 1024 * 1024, Time: 300 * time.Second}}
	env := adapter.SandboxEnv{Worktree: spec.Worktree, ConfigDir: spec.ConfigDir, Env: spec.Env}
	resolve := func(e adapter.SandboxEnv) (sandbox.SandboxSpec, error) {
		s := spec
		s.ConfigDir, s.Env = e.ConfigDir, e.Env
		return s, nil
	}
	var adapters adapter.Registry
	switch cli.name {
	case "codex":
		err = adapters.Register(cli.name, codex.New(codex.Config{ResolveAuthentication: func(adapter.HarnessRequest) (string, error) { return store, nil }, Launch: codex.ContainerLaunch(config, resolve), CancelGrace: 3 * time.Second,
			ResolveConfig: func(adapter.SandboxEnv) (*harnessenv.Environment, error) { return settings, nil },
			Configure: func(context.Context, adapter.HarnessRequest) ([]string, error) {
				// The fixture permits shell/commit inside the outer Sandbox only.
				return []string{`approval_policy="never"`}, nil
			}}))
	case "claude_code":
		err = adapters.Register(cli.name, claudecode.New(claudecode.Config{ResolveConfig: func(adapter.SandboxEnv) (*harnessenv.Environment, error) { return settings, nil }, ResolveAuthentication: func(adapter.HarnessRequest) (string, error) { return store, nil }, Launch: claudecode.ContainerLaunch(config, resolve), CancelGrace: 3 * time.Second,
			Configure: func(context.Context, adapter.HarnessRequest) (json.RawMessage, error) {
				return json.RawMessage(`{"permissions":{"allow":["Bash"],"deny":["Read","Edit","Write","WebFetch","WebSearch","Agent"]}}`), nil
			}}))
	default:
		t.Fatalf("unwired real Adapter %s", cli.name)
	}
	if err != nil {
		t.Fatal(err)
	}
	// Same product configuration/spec as both Adapter preflight and model CLI.
	// Deterministic access attempts are not entrusted to a model's self-report.
	checkConfigurationAccess(t, ctx, config, spec, f.Home, cli.instruction)
	version := configurationCommand(t, ctx, config, spec, []string{map[string]string{"codex": "codex", "claude_code": "claude"}[cli.name], "--version"})
	t.Logf("real CLI version: %s", strings.TrimSpace(string(version)))
	request := configurationRequest(f, profile)
	var result adapter.HarnessResult
	var logs []adapter.LogLine
	handlers := make(map[runner.StageID]runner.StageFunc)
	for _, stage := range runner.ExecutionStages() {
		// Other stages are fixture boundaries. This test exercises the real
		// Executor's harness stage, not setup, Gates, publishing or HTTP claim.
		handlers[stage.ID] = func(context.Context, *runner.Execution) error { return nil }
	}
	handlers[runner.StageHarness] = func(ctx context.Context, execution *runner.Execution) error {
		if execution.Claim.Execution.ID != request.ExecutionID {
			return fmt.Errorf("Execution ID mismatch")
		}
		h, err := adapters.Lookup(execution.Claim.Profile.Adapter)
		if err != nil {
			return err
		}
		id, err := h.Start(ctx, request, env)
		if err != nil {
			return err
		}
		defer func() { _ = h.Cancel(id) }()
		stream, err := h.Logs(id)
		if err != nil {
			_ = h.Cancel(id)
			_, _ = h.Result(id)
			return err
		}
		for line := range stream {
			logs = append(logs, line)
		}
		result, err = h.Result(id) // Waits for all CLI/Sandbox writers to stop.
		execution.HarnessResult = &result
		if err != nil {
			return err
		}
		return configurationVerdict(result, logs, a, b, f.Home)
	}
	executor, err := runner.NewExecutor(handlers)
	if err != nil {
		t.Fatal(err)
	}
	var claim runner.Claim
	claim.Execution.ID, claim.Profile.Adapter = request.ExecutionID, cli.name
	if err := executor.Execute(ctx, claim); err != nil {
		// Raw output may include authentication details. Do not dump it.
		class := "none"
		if result.ErrorClass != nil {
			class = string(*result.ErrorClass)
		}
		t.Fatalf("configuration isolation failed: %v; status=%s errorClass=%s; detail=%v (raw CLI logs withheld)", err, result.Status, class, configurationVerdict(result, logs, a, b, f.Home))
	}
	if _, err := settings.Files(ctx); err != nil {
		t.Fatalf("dedicated configuration changed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(f.Worktree, "isolation.txt")); err != nil || strings.TrimSpace(string(data)) != "configuration checked" {
		t.Fatalf("real CLI did not create requested fixture file: %v", err)
	}
	commits, err := f.Git(ctx, "rev-list", f.BaseSHA+"..HEAD")
	if err != nil || commits == "" {
		t.Fatalf("real CLI made no commit: %v", err)
	}
	status, err := f.Git(ctx, "status", "--porcelain", "--", "isolation.txt")
	if err != nil || status != "" {
		t.Fatalf("fixture file was not committed: %v", err)
	}
	checkConfigurationAccess(t, ctx, config, spec, f.Home, cli.instruction)
	t.Log("A absent from all CLI output; B present in final response; dedicated configuration unchanged; personal HOME denied")
	t.Log("complete CLI-reported loaded configuration list unavailable through the adopted Adapter output; see README §3.1")
}

func configurationCommand(t *testing.T, ctx context.Context, config sandbox.ContainerConfig, spec sandbox.SandboxSpec, command []string) []byte {
	t.Helper()
	var stdout bytes.Buffer
	config.Stdout = &stdout // Never print raw stderr/authentication data.
	container, err := sandbox.NewContainer(config)
	if err != nil {
		t.Fatalf("実行不能: %v", err)
	}
	process, err := container.Run(ctx, spec, command)
	if err != nil {
		t.Fatalf("実行不能: Harness Sandbox: %v", err)
	}
	err = process.Wait()
	code, known := process.ExitCode()
	if err != nil || !known || code != 0 {
		t.Fatalf("実行不能: Harness Sandbox command failed (exit=%d confirmed=%t): %v", code, known, err)
	}
	return stdout.Bytes()
}

func checkConfigurationAccess(t *testing.T, ctx context.Context, config sandbox.ContainerConfig, spec sandbox.SandboxSpec, home, instruction string) {
	t.Helper()
	paths := []string{home}
	for _, name := range personalInstructionPaths {
		paths = append(paths, filepath.Join(home, name))
	}
	command := append([]string{"python3", "-c", configurationAccessScript, home, filepath.Join(spec.ConfigDir, instruction)}, paths...)
	data := configurationCommand(t, ctx, config, spec, command)
	if err := configurationAccessVerdict(data, paths); err != nil {
		t.Fatal(err)
	}
}
