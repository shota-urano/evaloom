package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
)

// Inspect the actual runtime invocation produced by Container.Run, rather than
// inferring isolation from a HOME variable alone. The runtime is a test double;
// these tests do not claim real CLI canary or container breakout coverage.
func TestDedicatedHarnessConfigurationSandboxBoundary(t *testing.T) {
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		t.Run(variable, func(t *testing.T) {
			f := newFixture(t, "env-probe")
			for _, name := range []string{".codex/AGENTS.md", ".claude/CLAUDE.md", ".gitconfig"} {
				p := filepath.Join(f.config.HostHome, name)
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("private-home-canary"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			h := sha256.Sum256([]byte("profile-canary"))
			env, err := harnessenv.Create(context.Background(), harnessenv.Config{Root: f.config.ConfigDirRoot,
				CommitSHA: strings.Repeat("b", 40), ConfigVariable: variable,
				Files: []gitcontext.File{{Path: "AGENTS.md", Content: "profile-canary", ContentHash: hex.EncodeToString(h[:]), SourceCommitSHA: strings.Repeat("b", 40)}}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = env.Close() })
			f.spec.ConfigDir, f.spec.Env = env.ConfigDir(), env.Env()
			f.spec.Env["HOME"] = f.config.HostHome
			f.spec.Env["RUNNER_TOKEN"] = "secret-token"
			f.spec.Env["SSH_AUTH_SOCK"] = "/private-agent"
			f.config.AllowEnv = []string{variable}
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/usr/bin/env"})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Wait(); err != nil {
				t.Fatal(err)
			}
			lines := strings.Fields(f.stdout.String())
			slices.Sort(lines)
			want := []string{"HOME=" + privateHome, variable + "=" + env.ConfigDir()}
			slices.Sort(want)
			if !slices.Equal(lines, want) {
				t.Fatalf("effective environment=%v; want %v", lines, want)
			}
			for _, call := range f.calls(t) {
				if !slices.Contains(call.Args, "create") {
					continue
				}
				var mounts, tmpfs []string
				for i, arg := range call.Args {
					switch arg {
					case "--mount":
						mounts = append(mounts, call.Args[i+1])
					case "--tmpfs":
						tmpfs = append(tmpfs, call.Args[i+1])
					}
				}
				realConfig, err := filepath.EvalSymlinks(env.ConfigDir())
				if err != nil {
					t.Fatal(err)
				}
				if len(mounts) != 3 || !slices.Contains(mounts, "type=bind,source="+realConfig+",target="+env.ConfigDir()+",readonly") ||
					!slices.Contains(tmpfs, privateHome+":rw,nosuid,nodev,noexec") {
					t.Fatalf("configuration or empty HOME mount missing: %v %v", mounts, tmpfs)
				}
				for _, mount := range mounts {
					if strings.Contains(mount, f.config.HostHome) || strings.Contains(mount, f.config.DataDir) || strings.Contains(mount, f.config.RunnerTokenFile) {
						t.Fatalf("protected host files exposed: %s", mount)
					}
				}
			}
		})
	}
}

// Opt-in acceptance probe through the production Work Sandbox, with no runtime
// or CLI-launch replacement. It needs only a preinstalled shell image, no model.
func TestDockerDedicatedHarnessConfiguration(t *testing.T) {
	if os.Getenv("EVALOOM_SANDBOX_DOCKER_TEST") != "1" {
		t.Skip("set EVALOOM_SANDBOX_DOCKER_TEST=1 to run real Docker HOME/configuration probes")
	}
	image := os.Getenv("EVALOOM_SANDBOX_TEST_IMAGE")
	if image == "" {
		t.Fatal("EVALOOM_SANDBOX_TEST_IMAGE must name an installed Linux image with sh, cat and find")
	}
	runtime, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	for _, cli := range []struct{ variable, name string }{{"CODEX_HOME", "AGENTS.md"}, {"CLAUDE_CONFIG_DIR", "CLAUDE.md"}} {
		t.Run(cli.variable, func(t *testing.T) {
			f := stageFixture(t, "normal", Work)
			f.config.RuntimePath, f.config.Image = runtime, image
			f.config.ControlTimeout = 10 * time.Second
			f.config.RuntimeEnv = map[string]string{}
			for _, key := range []string{"HOME", "PATH", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
				if value, ok := os.LookupEnv(key); ok {
					f.config.RuntimeEnv[key] = value
				}
			}
			for _, name := range []string{".codex/AGENTS.md", ".claude/CLAUDE.md", ".gitconfig"} {
				p := filepath.Join(f.config.HostHome, name)
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("private-home-canary"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			commit := strings.Repeat("b", 40)
			digest := sha256.Sum256([]byte("profile-canary"))
			env, err := harnessenv.Create(t.Context(), harnessenv.Config{Root: f.config.ConfigDirRoot,
				CommitSHA: commit, ConfigVariable: cli.variable, Instructions: []string{"work/instructions.md"},
				Files: []gitcontext.File{{Path: "work/instructions.md", Content: "profile-canary", ContentHash: hex.EncodeToString(digest[:]), SourceCommitSHA: commit}}})
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			f.spec.ConfigDir, f.spec.Env = env.ConfigDir(), env.Env()
			f.spec.Env["HOME"] = f.config.HostHome
			f.spec.Env["RUNNER_TOKEN"] = "must-not-be-forwarded"
			f.spec.Limits.Time = 30 * time.Second
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			// Run twice to prove HOME is fresh even when configuration is shared.
			for range 2 {
				p, err := f.sandbox(t).RunControlled(ctx, f.spec, []string{"/bin/sh", "-ec", `
test "$HOME" = /evaloom-home
test -z "$(find "$HOME" -mindepth 1 -print)"
test ! -e "$2"
test -z "${RUNNER_TOKEN:-}"
test -z "${SSH_AUTH_SOCK:-}"
test "$(cat "$1/$3")" = profile-canary
test "$(cat "$1/work/instructions.md")" = profile-canary
test "$(find "$1" -type f | wc -l)" -eq 2
test ! -e /job/profile
if touch "$1/undeclared"; then exit 41; fi
printf state > "$HOME/state"
`, "probe", env.ConfigDir(), f.config.HostHome, cli.name})
				if err != nil {
					t.Fatal(err)
				}
				if err := p.Wait(); err != nil {
					t.Fatalf("configuration/HOME probe: %v; stdout=%s stderr=%s", err, f.stdout.String(), f.stderr.String())
				}
				if code, ok := p.ExitCode(); !ok || code != 0 {
					t.Fatalf("configuration/HOME probe exit=%d confirmed=%t; stdout=%s stderr=%s", code, ok, f.stdout.String(), f.stderr.String())
				}
			}
		})
	}
}
