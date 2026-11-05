//go:build security_docker

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
)

// Exercises Docker's stopped-container tar transfer into a named volume with
// a read-only root filesystem. No harness CLI, auth or model API is used.
func TestDockerWorkingConfiguration(t *testing.T) {
	image := os.Getenv("EVALOOM_SECURITY_TEST_IMAGE")
	if image == "" {
		t.Fatal("EVALOOM_SECURITY_TEST_IMAGE must name an installed Linux image with sh and cat")
	}
	f := newFixture(t, "normal")
	runtime, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	f.config.RuntimePath, f.config.Image = runtime, image
	f.config.ControlTimeout = 10 * time.Second
	f.config.RuntimeEnv = map[string]string{}
	for _, key := range []string{"HOME", "PATH", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		if value, ok := os.LookupEnv(key); ok {
			f.config.RuntimeEnv[key] = value
		}
	}
	f.config.AllowEnv = []string{"CODEX_HOME"}
	original := workingSettings(t, f.config.ConfigDirRoot)
	copy, err := original.WorkingCopy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := copy.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx, closeConfig, err := WithWorkingConfiguration(ctx, copy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeConfig(); err != nil {
			t.Error(err)
		}
	})
	w := ctx.Value(configurationKey{}).(*workingConfiguration)
	f.spec.ConfigDir, f.spec.ConfigWritable, f.spec.Env = copy.ConfigDir(), true, copy.Env()
	f.spec.Limits.Time = 30 * time.Second
	c := f.sandbox(t)
	for _, command := range []string{
		`test "$(cat "$CODEX_HOME/config.toml")" = 'pinned auth' && test -f "$CODEX_HOME/skills/check/SKILL.md" && test ! -e "$1" && printf 'CLI state' > "$CODEX_HOME/runtime-state" && ! touch /evaloom-forbidden-write`,
		`test "$(cat "$CODEX_HOME/runtime-state")" = 'CLI state' && printf 'updated auth' > "$CODEX_HOME/config.toml"`,
	} {
		p, err := c.RunControlled(ctx, f.spec, []string{"/bin/sh", "-ec", command, "test-config", original.ConfigDir()})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Wait(); err != nil {
			t.Fatal(err)
		}
		if code, ok := p.ExitCode(); !ok || code != 0 {
			t.Fatalf("workload exit=%d confirmed=%t", code, ok)
		}
	}
	// CLI writes belong only to the volume, never either host environment.
	for _, env := range []*harnessenv.Environment{original, copy} {
		if _, err := env.Files(ctx); err != nil {
			t.Fatal("CLI changed host config:", err)
		}
	}
	if err := closeConfig(); err != nil {
		t.Fatal(err)
	}
	inspect := c.command(ctx, "volume", "inspect", w.volume)
	if err := inspect.Run(); err == nil {
		t.Fatal("working volume survived cleanup")
	}
}

// Dedicated dummy credentials only. Both supported CLI directory variables use
// the same volume/recovery path without a model call or personal auth access.
func TestDockerAuthenticationWriteback(t *testing.T) {
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		t.Run(variable, func(t *testing.T) {
			image := os.Getenv("EVALOOM_SECURITY_TEST_IMAGE")
			if image == "" {
				t.Fatal("EVALOOM_SECURITY_TEST_IMAGE must name an installed Linux image with sh and cat")
			}
			f := newFixture(t, "normal")
			runtime, err := exec.LookPath("docker")
			if err != nil {
				t.Fatal(err)
			}
			f.config.RuntimePath, f.config.Image = runtime, image
			f.config.ControlTimeout = 10 * time.Second
			f.config.RuntimeEnv = map[string]string{"HOME": f.config.HostHome, "PATH": os.Getenv("PATH"), "DOCKER_CONFIG": filepath.Join(f.config.HostHome, ".docker")}
			if value := os.Getenv("DOCKER_HOST"); value != "" {
				f.config.RuntimeEnv["DOCKER_HOST"] = value
			}
			f.config.AllowEnv = []string{variable}
			original, err := harnessenv.Create(context.Background(), harnessenv.Config{Root: f.config.ConfigDirRoot, CommitSHA: strings.Repeat("a", 40), ConfigVariable: variable})
			if err != nil {
				t.Fatal(err)
			}
			defer original.Close()
			working, err := original.WorkingCopy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer working.Close()
			store := filepath.Join(t.TempDir(), "credential")
			if err := os.WriteFile(store, []byte(`{"token":"dummy-original"}`), 0600); err != nil {
				t.Fatal(err)
			}
			lease, err := harnessenv.AcquireAuthentication(context.Background(), store)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if err := lease.Install(working); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			ctx, closeConfig, err := WithWorkingConfiguration(ctx, working)
			if err != nil {
				t.Fatal(err)
			}
			defer closeConfig()
			f.spec.ConfigDir, f.spec.ConfigWritable, f.spec.Env = working.ConfigDir(), true, working.Env()
			f.spec.Limits.Time = 30 * time.Second
			name := "auth.json"
			if variable == "CLAUDE_CONFIG_DIR" {
				name = ".credentials.json"
			}
			c := f.sandbox(t)
			p, err := c.RunControlled(ctx, f.spec, []string{"/bin/sh", "-ec", `test "$(cat "$1/$2")" = '{"token":"dummy-original"}' && printf '%s' '{"token":"dummy-refreshed"}' > "$1/$2"`, "test-auth", working.ConfigDir(), name})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Wait(); err != nil {
				t.Fatal(err)
			}
			// Recovery must still work with an already-cancelled execution context.
			cancelled, stop := context.WithCancel(ctx)
			stop()
			if err := WriteBackAuthentication(cancelled); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(store)
			if err != nil || string(got) != `{"token":"dummy-refreshed"}` {
				t.Fatal("Docker credential refresh lost", err)
			}
			if _, err := os.Stat(filepath.Join(original.ConfigDir(), name)); !os.IsNotExist(err) {
				t.Fatal("credential appeared in original config")
			}
		})
	}
}
