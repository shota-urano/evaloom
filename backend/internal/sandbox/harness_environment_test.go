package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
