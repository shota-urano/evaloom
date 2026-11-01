package codex

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

func TestContainerLaunchUsesPreparedHarnessSandbox(t *testing.T) {
	f := newFixture(t, "success")
	root := filepath.Dir(f.env.Worktree)
	paths := map[string]string{}
	for _, name := range []string{"personal-home", "data", "git", "config-root"} {
		paths[name] = filepath.Join(root, name)
		if err := os.Mkdir(paths[name], 0700); err != nil {
			t.Fatal(err)
		}
	}
	capture := filepath.Join(root, "runtime-args")
	runtime := filepath.Join(root, "runtime")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" >> "$CAPTURE"
case "$1:$2" in
  image:inspect) printf '%s\n' '{"Os":"linux","Config":{}}' ;;
  container:create) exit 0 ;;
  container:start) printf '%s\n' '{"type":"turn.completed"}' ;;
  container:inspect) printf '%s\n' '{"Status":"exited","Running":false,"ExitCode":0}' ;;
  container:rm) exit 0 ;;
  *) exit 19 ;;
esac
`
	if err := os.WriteFile(runtime, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config := sandbox.ContainerConfig{RuntimePath: runtime, Image: "test-codex-image", RuntimeEnv: map[string]string{"CAPTURE": capture}, AllowEnv: []string{"CODEX_HOME", "PATH"}, WorktreeRoot: root, GitDirRoot: paths["git"], ConfigDirRoot: root, HostHome: paths["personal-home"], DataDir: paths["data"], RunnerTokenFile: filepath.Join(paths["data"], "token"), ControlTimeout: time.Second}
	scope := func(env adapter.SandboxEnv) (sandbox.SandboxSpec, error) {
		return sandbox.SandboxSpec{Kind: sandbox.Harness, Worktree: env.Worktree, ConfigDir: env.ConfigDir, GitDir: paths["git"], Env: env.Env, Network: sandbox.NetworkNone, Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 128 << 20, Time: time.Second}}, nil
	}
	f.config.Launch = ContainerLaunch(config, scope)
	f.h = New(f.config)
	_, result, _ := runResult(t, f)
	if result.Status != adapter.ResultSucceeded {
		t.Fatalf("result=%+v", result)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(data), "\n")
	for _, want := range []string{"--read-only", "--network", "none", "--workdir", f.env.Worktree, "HOME=/evaloom-home", "CODEX_HOME=" + f.env.ConfigDir, "type=bind,source=" + f.env.ConfigDir + ",target=" + f.env.ConfigDir + ",readonly", "--json", "login", "status"} {
		if !slices.Contains(args, want) {
			t.Fatalf("runtime missing %q: %s", want, data)
		}
	}
	if strings.Count(string(data), "container\ncreate\n") != 2 {
		t.Fatalf("preflight and exec not both sandboxed: %s", data)
	}
	bad := f.env
	bad.ConfigDir = "/wrong"
	launch := ContainerLaunch(config, scope)
	// The resolver deliberately returns a different worktree than the request.
	badLaunch := ContainerLaunch(config, func(env adapter.SandboxEnv) (sandbox.SandboxSpec, error) {
		spec, _ := scope(env)
		spec.Worktree = "/wrong"
		return spec, nil
	})
	if _, err := badLaunch(context.Background(), f.env, []string{"codex"}, io.Discard, io.Discard); err == nil {
		t.Fatal("scope mismatch accepted")
	}
	if _, err := launch(context.Background(), bad, []string{"codex"}, io.Discard, io.Discard); err == nil {
		t.Fatal("invalid config mount accepted")
	}
}
