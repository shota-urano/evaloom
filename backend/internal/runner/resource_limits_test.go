package runner

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

func TestConfigResourceLimitsReachSandboxRuntime(t *testing.T) {
	for _, tt := range []struct {
		name       string
		values     map[string]any
		wantCPU    string
		wantMemory string // Bytes, as required by the runtime's --memory flag.
	}{
		{"omitted", nil, "4", "8589934592"},
		{"cpu only", map[string]any{"cpus": 2.5}, "2.5", "8589934592"},
		{"memory only", map[string]any{"memoryMiB": 1536}, "4", "1610612736"},
		{"explicit", map[string]any{"cpus": 0.5, "memoryMiB": 1025}, "0.5", "1074790400"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			runtime := filepath.Join(root, "runtime")
			// Record the actual create argv while exercising the production
			// Sandbox.Run path, including image inspection and process cleanup.
			if err := os.WriteFile(runtime, []byte(`#!/bin/sh
case "$1 $2" in
  'image inspect') printf '%s' '{"Os":"linux","Config":{}}' ;;
  'container create') printf '%s\n' "$@" > "$RESOURCE_ARGS" ;;
  'container inspect') printf '%s' '{"Status":"exited","Running":false,"ExitCode":0}' ;;
  'container start'|'container rm') ;;
  *) exit 99 ;;
esac
`), 0700); err != nil {
				t.Fatal(err)
			}
			document := configDocument(t)
			container := document["container"].(map[string]any)
			delete(container, "cpus")
			delete(container, "memoryMiB")
			for key, value := range tt.values {
				container[key] = value
			}
			argsPath := filepath.Join(root, "create-args")
			container["runtimePath"] = runtime
			container["runtimeEnv"] = map[string]string{"RESOURCE_ARGS": argsPath}
			config, err := LoadConfig(writeConfigDocument(t, document))
			if err != nil {
				t.Fatal(err)
			}
			worktree, gitDir, configDir := filepath.Join(root, "worktree"), filepath.Join(root, "git"), filepath.Join(root, "config")
			for _, path := range []string{worktree, gitDir, configDir} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			isolated, err := sandbox.NewContainer(sandbox.ContainerConfig{
				RuntimePath: config.Container.RuntimePath, Image: config.Container.Image, RuntimeEnv: config.Container.RuntimeEnv,
				WorktreeRoot: worktree, GitDirRoot: gitDir, ConfigDirRoot: configDir,
				HostHome: filepath.Join(root, "host-home"), DataDir: config.DataDir, RunnerTokenFile: config.TokenFile,
				ControlTimeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, kind := range []sandbox.SandboxKind{sandbox.Harness} {
				t.Run(string(kind), func(t *testing.T) {
					limits := config.Container.ResourceLimits(time.Minute)
					if limits.Time != time.Minute {
						t.Fatalf("stage timeout changed: %v", limits.Time)
					}
					spec := sandbox.SandboxSpec{Kind: kind, Worktree: worktree, Limits: limits}
					if kind == sandbox.Harness {
						spec.GitDir, spec.ConfigDir = gitDir, configDir
					}
					process, err := isolated.Run(context.Background(), spec, []string{"/bin/true"})
					if err != nil {
						t.Fatal(err)
					}
					if err := process.Wait(); err != nil {
						t.Fatal(err)
					}
					content, err := os.ReadFile(argsPath)
					if err != nil {
						t.Fatal(err)
					}
					args := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
					for flag, want := range map[string]string{"--cpus": tt.wantCPU, "--memory": tt.wantMemory, "--memory-swap": tt.wantMemory} {
						index := slices.Index(args, flag)
						if index < 0 || index+1 >= len(args) || args[index+1] != want || slices.Contains(args[index+1:], flag) {
							t.Errorf("want exactly one %s %s, got %q", flag, want, args)
						}
					}
				})
			}
		})
	}
}

func TestConfigRejectsInvalidResourceLimits(t *testing.T) {
	for key, values := range map[string][]any{
		"cpus":      {0, -1, -0.5, nil, "4", true},
		"memoryMiB": {0, -1, 0.5, nil, "8192", true, int64(math.MaxInt64/(1<<20) + 1)},
	} {
		for _, value := range values {
			document := configDocument(t)
			document["container"].(map[string]any)[key] = value
			if _, err := LoadConfig(writeConfigDocument(t, document)); err == nil {
				t.Errorf("accepted container.%s=%v", key, value)
			}
		}
	}
	for _, cpus := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		config := testConfig(t, "https://control.example.test")
		config.Container.CPUs = cpus
		if _, err := New(config); err == nil {
			t.Errorf("accepted programmatic CPU limit %v", cpus)
		}
	}
}
