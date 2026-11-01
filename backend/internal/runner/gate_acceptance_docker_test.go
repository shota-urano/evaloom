//go:build security_docker

package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/securitytest"
)

// Missing prerequisites FAIL; there is no skip or host-process fallback.
func TestDockerGateAcceptance(t *testing.T) {
	image := os.Getenv("EVALOOM_SECURITY_TEST_IMAGE")
	if image == "" {
		t.Fatal("実行不能: EVALOOM_SECURITY_TEST_IMAGE must name a prebuilt image (see securitytest/Dockerfile.gates)")
	}
	runtimePath, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("実行不能: %v", err)
	}
	runtimePath, err = filepath.Abs(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	runtimeEnv := map[string]string{}
	for _, key := range []string{"HOME", "PATH", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		if value, ok := os.LookupEnv(key); ok {
			runtimeEnv[key] = value
		}
	}
	runGateAcceptance(t, func(t *testing.T, f *securitytest.Fixture, common string) (probe.Sandbox, sandbox.Sandbox) {
		root := filepath.Dir(f.Worktree)
		configDir, dataDir := filepath.Join(root, "config"), filepath.Join(root, "data")
		for _, path := range []string{configDir, dataDir} {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		}
		vars := map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "PYTHONDONTWRITEBYTECODE": "1",
			"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_TERMINAL_PROMPT": "0",
			"GIT_AUTHOR_NAME": "Evaloom Test", "GIT_AUTHOR_EMAIL": "evaloom-test@example.invalid",
			"GIT_COMMITTER_NAME": "Evaloom Test", "GIT_COMMITTER_EMAIL": "evaloom-test@example.invalid"}
		allow := []string{"EVALOOM_GATE_CASE"}
		for key := range vars {
			allow = append(allow, key)
		}
		container, err := sandbox.NewContainer(sandbox.ContainerConfig{
			RuntimePath: runtimePath, Image: image, RuntimeEnv: runtimeEnv, AllowEnv: allow,
			WorktreeRoot: f.Worktree, GitDirRoot: common, ConfigDirRoot: configDir,
			HostHome: f.Home, DataDir: dataDir, RunnerTokenFile: filepath.Join(dataDir, "runner-token"),
			ControlTimeout: 15 * time.Second, Stdout: os.Stdout, Stderr: os.Stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		provider := &gitDefenseDocker{container: container, spec: sandbox.SandboxSpec{
			Kind: sandbox.Harness, Worktree: f.Worktree, GitDir: common, ConfigDir: configDir,
			Env: vars, Network: sandbox.NetworkNone,
			Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 256 << 20, Time: time.Minute},
		}}
		// Check Node before baseline acquisition; Python/Git are checked by Check.
		out, err := provider.Run(context.Background(), adapter.SandboxEnv{Worktree: f.Worktree}, []string{"node", "--version"})
		if err != nil || out.ExitCode != 0 {
			t.Fatalf("実行不能: Node: exit=%d err=%v", out.ExitCode, err)
		}
		return provider, container
	})
}
