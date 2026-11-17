package runner

import (
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

func TestWorkLaunchPreservesStageEnvironment(t *testing.T) {
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		t.Run(variable, func(t *testing.T) {
			config := testConfig(t, "https://control.example.test")
			root := filepath.Dir(config.DataDir)
			directory := filepath.Join(config.DataDir, "executions", "execution", "work-1")
			configuration := filepath.Join(directory, "configuration")
			profile := filepath.Join(config.DataDir, "profiles", "pinned")
			workspace := filepath.Join(config.WorkspaceRoot, "workspace")
			for _, path := range []string{profile, workspace, configuration} {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"out", "inputs", "scratch"} {
				if err := os.Mkdir(filepath.Join(directory, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(directory, "request.json"), []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			argsPath := filepath.Join(root, "create-args")
			config.Container.RuntimePath = filepath.Join(root, "runtime")
			config.Container.RuntimeEnv = map[string]string{"WORK_RUNTIME_ROOT": root}
			// Only the runtime is replaced. Record the final env -i invocation;
			// preserve configuration transfer/readback and process supervision.
			if err := os.WriteFile(config.Container.RuntimePath, []byte(`#!/bin/sh
case "$1 $2" in
  'image inspect') printf '%s' '{"Os":"linux","Config":{}}' ;;
  'volume create'|'volume rm') ;;
  'container create') printf '%s\n' "$@" > "$WORK_RUNTIME_ROOT/create-args" ;;
  'container cp')
    if [ "$3" = '-' ]; then
      /bin/cat > "$WORK_RUNTIME_ROOT/configuration.tar"
    else
      /bin/cat "$WORK_RUNTIME_ROOT/configuration.tar"
    fi ;;
  'container inspect') printf '%s' '{"Status":"exited","Running":false,"ExitCode":0}' ;;
  'container start'|'container rm') ;;
  *) exit 99 ;;
esac
`), 0700); err != nil {
				t.Fatal(err)
			}
			environment, err := harnessenv.Create(t.Context(), harnessenv.Config{
				Root: configuration, CommitSHA: strings.Repeat("a", 40), ConfigVariable: variable,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer environment.Close()
			working, err := environment.WorkingCopy(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer working.Close()
			ctx, closeConfiguration, err := sandbox.WithWorkingConfiguration(t.Context(), working)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := closeConfiguration(); err != nil {
					t.Error(err)
				}
			}()
			e := &executionExecutor{config: config, paths: DefaultProposalPaths, adapters: &adapter.Registry{}}
			spec, container, err := e.stageSandbox(StageInvocation{
				Stage: model.StageWork, Directory: directory, Workspace: workspace,
				Profile: LoadedProfile{Path: profile}, timeout: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			container.ConfigDirRoot = configuration
			e.workSpec, e.container = spec, container
			env := adapter.SandboxEnv{Worktree: e.paths.Workspace, ConfigDir: working.ConfigDir(), Env: working.Env()}
			baseEnv, adapterEnv := maps.Clone(spec.Env), maps.Clone(env.Env)
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			process, err := e.workAdapterConfig().Launch(ctx, env, []string{"codex", "exec"}, io.Discard, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if err := process.Wait(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			index := slices.Index(args, "-i")
			if index < 0 {
				t.Fatalf("runtime did not clear ambient environment: %q", args)
			}
			for _, entry := range []string{
				"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8",
				"EVALOOM_REQUEST=/evaloom/request.json", variable + "=" + working.ConfigDir(),
			} {
				if !slices.Contains(args[index+1:], entry) {
					t.Errorf("work Launch lost %s: %q", entry, args[index+1:])
				}
			}
			if !maps.Equal(e.workSpec.Env, baseEnv) || !maps.Equal(env.Env, adapterEnv) {
				t.Error("work Launch mutated the stage or Adapter environment")
			}
		})
	}
}
