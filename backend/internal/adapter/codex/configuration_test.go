package codex

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

func TestCodexRejectsCopyMismatchBeforeAnyCLI(t *testing.T) {
	for _, mode := range []string{"changed", "extra", "missing", "symlink", "directory_replacement", "original_symlink", "missing_resolver", "resolver_error", "wrong_source", "source_changed"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, "success")
			var displaced string
			launched := false
			f.config.Launch = func(context.Context, adapter.SandboxEnv, []string, io.Writer, io.Writer) (sandbox.Process, error) {
				launched = true
				return nil, errors.New("must not launch even login status")
			}
			switch mode {
			case "missing_resolver":
				f.config.ResolveConfig = nil
			case "resolver_error":
				f.config.ResolveConfig = func(adapter.SandboxEnv) (*harnessenv.Environment, error) { return nil, errors.New("unavailable") }
			case "wrong_source":
				f.env.ConfigDir = filepath.Dir(f.env.ConfigDir)
				f.env.Env["CODEX_HOME"] = f.env.ConfigDir
			case "source_changed":
				if err := os.WriteFile(filepath.Join(f.settings.ConfigDir(), "config.toml"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				// Configure runs after copy creation and before the launch-boundary
				// check. Mutate the actual copy, not its asserted manifest.
				f.config.Configure = func(context.Context, adapter.HarnessRequest) ([]string, error) {
					entries, err := os.ReadDir(filepath.Dir(f.settings.ConfigDir()))
					if err != nil {
						return nil, err
					}
					for _, entry := range entries {
						if !strings.HasPrefix(entry.Name(), "harness-") || entry.Name() == filepath.Base(f.settings.ConfigDir()) {
							continue
						}
						dir := filepath.Join(filepath.Dir(f.settings.ConfigDir()), entry.Name())
						name := filepath.Join(dir, "config.toml")
						switch mode {
						case "directory_replacement", "original_symlink":
							displaced = dir + "-displaced"
							if err = os.Rename(dir, displaced); err != nil {
								return nil, err
							}
							if mode == "original_symlink" {
								err = os.Symlink(f.settings.ConfigDir(), dir)
							} else {
								err = os.Mkdir(dir, 0700)
								if err == nil {
									err = os.WriteFile(name, []byte("unrecorded replacement"), 0600)
								}
							}
						case "changed":
							err = os.WriteFile(name, []byte("changed"), 0600)
						case "extra":
							err = os.WriteFile(filepath.Join(dir, "unrecorded"), []byte("extra"), 0600)
						case "missing":
							err = os.Remove(name)
						case "symlink":
							err = os.Symlink(filepath.Join(f.settings.ConfigDir(), "config.toml"), filepath.Join(dir, "link"))
						}
						if err != nil {
							t.Error(err)
						}
						return nil, err
					}
					t.Error("copy not created before configuration")
					return nil, errors.New("copy missing")
				}
			}
			f.h = New(f.config)
			_, result, _ := runResultAllowReplacement(t, f)
			if launched || result.ErrorClass == nil || *result.ErrorClass != adapter.ErrorConfig {
				t.Fatalf("launched=%t result=%+v", launched, result)
			}
			if displaced != "" {
				if _, err := os.Lstat(displaced); !os.IsNotExist(err) {
					t.Fatalf("displaced generated copy survived Result: %v", err)
				}
				if _, err := f.settings.Files(context.Background()); err != nil {
					t.Fatal("original changed:", err)
				}
			} else {
				assertWorkingCopiesRemoved(t, f)
			}
		})
	}
}

type configurationProcess struct {
	done    <-chan struct{}
	stopped chan<- struct{}
}

func (p configurationProcess) Wait() error { <-p.done; return nil }
func (p configurationProcess) Stop() error {
	select {
	case p.stopped <- struct{}{}:
	default:
	}
	return nil
}
func (p configurationProcess) Kill() error           { return p.Stop() }
func (p configurationProcess) ExitCode() (int, bool) { return 0, true }

func TestCodexCopyRemainsUntilSandboxStopConfirmed(t *testing.T) {
	f := newFixture(t, "success")
	finished, stopped, started := make(chan struct{}), make(chan struct{}, 2), make(chan string, 1)
	var release sync.Once
	defer release.Do(func() { close(finished) })
	calls := 0
	f.config.Launch = func(_ context.Context, env adapter.SandboxEnv, _ []string, _, _ io.Writer) (sandbox.Process, error) {
		calls++
		if calls == 1 {
			done := make(chan struct{})
			close(done)
			return configurationProcess{done: done}, nil
		}
		started <- env.ConfigDir
		return configurationProcess{done: finished, stopped: stopped}, nil
	}
	f.h = New(f.config)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := f.h.Start(ctx, f.request, f.env)
	if err != nil {
		t.Fatal(err)
	}
	var directory string
	select {
	case directory = <-started:
	case <-ctx.Done():
		t.Fatal("exec never started")
	}
	if err := f.h.Cancel(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("sandbox never received stop")
	}
	if _, err := os.Stat(filepath.Join(directory, "config.toml")); err != nil {
		t.Fatal("copy removed before Wait confirmed stop:", err)
	}
	if status, err := f.h.Status(id); err != nil || status != adapter.SessionRunning {
		t.Fatalf("finished before stop confirmed: %s %v", status, err)
	}
	release.Do(func() { close(finished) })
	result, err := f.h.Result(id)
	if err != nil || result.ErrorClass == nil || *result.ErrorClass != adapter.ErrorCancelled {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	assertWorkingCopiesRemoved(t, f)
	if _, err := f.settings.Files(context.Background()); err != nil {
		t.Fatal("cancellation changed original configuration:", err)
	}
}

func TestCodexWorkingCopyLifetime(t *testing.T) {
	for _, mode := range []string{"success", "exit", "config", "missing-cli", "launch_failure", "policy_failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, mode)
			launch := f.config.Launch
			var directory string
			calls := 0
			f.config.Launch = func(ctx context.Context, env adapter.SandboxEnv, args []string, out, errOut io.Writer) (sandbox.Process, error) {
				calls++
				if env.ConfigDir == f.settings.ConfigDir() || env.Env["CODEX_HOME"] != env.ConfigDir || (directory != "" && directory != env.ConfigDir) {
					t.Error("CLI did not receive the same isolated copy")
				}
				directory = env.ConfigDir
				name := filepath.Join(directory, "config.toml")
				if _, err := os.Stat(name); err != nil {
					t.Error("copy missing while launching:", err)
				}
				// login status may create runtime files; exec must reuse them
				// without re-pinning them as configuration or rejecting them.
				state := filepath.Join(directory, "runtime-state")
				if calls == 1 {
					if err := os.WriteFile(state, []byte("CLI state"), 0600); err != nil {
						t.Error(err)
					}
				} else if data, err := os.ReadFile(state); err != nil || string(data) != "CLI state" {
					t.Errorf("preflight state not retained: %s %v", data, err)
				}
				if mode == "launch_failure" {
					return nil, &sandbox.Error{Op: "start failed"}
				}
				return launch(ctx, env, args, out, errOut)
			}
			if mode == "policy_failure" {
				f.config.Configure = func(context.Context, adapter.HarnessRequest) ([]string, error) {
					return nil, errors.New("policy failed")
				}
			}
			f.h = New(f.config)
			_, result, _ := runResult(t, f)
			if mode == "success" && (result.Status != adapter.ResultSucceeded || calls != 2) {
				t.Fatalf("result=%+v calls=%d", result, calls)
			}
			if mode != "success" && result.Status != adapter.ResultFailed {
				t.Fatalf("result=%+v", result)
			}
			if directory != "" {
				if _, err := os.Stat(directory); !os.IsNotExist(err) {
					t.Fatal("copy survived Result")
				}
			}
			if _, err := f.settings.Files(context.Background()); err != nil {
				t.Fatal("CLI writes changed recorded configuration:", err)
			}
		})
	}
}
