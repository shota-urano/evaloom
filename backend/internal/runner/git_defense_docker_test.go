//go:build security_docker

package runner

import (
	"context"
	"fmt"
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

// Selecting security_docker requires real Sandboxes. No prerequisite failure
// skips the test or falls back to the process doubles in TestHostGitDefense.
func TestDockerHostGitDefense(t *testing.T) {
	image := os.Getenv("EVALOOM_SECURITY_TEST_IMAGE")
	if image == "" {
		t.Fatal("実行不能: EVALOOM_SECURITY_TEST_IMAGE must name a prebuilt Linux image (see securitytest/Dockerfile.boundary)")
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
	runGitDefense(t, func(t *testing.T, f *securitytest.Fixture, common string) (probe.Sandbox, sandbox.Sandbox) {
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
		allow := []string{"EVALOOM_GIT_ATTACK"}
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
		// This deterministic probe needs no network. Only its worktree, managed
		// Git area and dedicated configuration are mounted in the Harness.
		return &gitDefenseDocker{container: container, spec: sandbox.SandboxSpec{
			Kind: sandbox.Harness, Worktree: f.Worktree, GitDir: common, ConfigDir: configDir,
			Env: vars, Network: sandbox.NetworkNone,
			Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 256 << 20, Time: time.Minute},
		}}, container
	})
}

// Only the test adapter uses this glue. Wait confirms container removal before
// the host reads the bind-mounted report or invokes Git against the armed area.
type gitDefenseDocker struct {
	container *sandbox.Container
	spec      sandbox.SandboxSpec
}

func (s *gitDefenseDocker) Check(ctx context.Context) error {
	out, err := s.Run(ctx, adapter.SandboxEnv{Worktree: s.spec.Worktree},
		[]string{"python3", "-c", "import subprocess; subprocess.run(['git', '--version'], check=True)"})
	if err != nil || out.ExitCode != 0 {
		return fmt.Errorf("実行不能: Docker probe prerequisites: exit=%d err=%v", out.ExitCode, err)
	}
	return nil
}

func (s *gitDefenseDocker) Run(ctx context.Context, env adapter.SandboxEnv, argv []string) (probe.Output, error) {
	if env.Worktree != s.spec.Worktree {
		return probe.Output{}, fmt.Errorf("probe worktree differs from Docker mount")
	}
	spec := s.spec
	spec.Env = make(map[string]string, len(s.spec.Env)+len(env.Env))
	for key, value := range s.spec.Env {
		spec.Env[key] = value
	}
	for key, value := range env.Env {
		spec.Env[key] = value
	}
	p, err := s.container.Run(ctx, spec, argv)
	if err != nil {
		return probe.Output{}, err
	}
	err = p.Wait()
	code, known := p.ExitCode()
	if !known {
		return probe.Output{}, fmt.Errorf("実行不能: Docker workload exit status unavailable: %v", err)
	}
	return probe.Output{ExitCode: code}, err
}

func (s *gitDefenseDocker) ReadFile(ctx context.Context, env adapter.SandboxEnv, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if env.Worktree != s.spec.Worktree || name != "harness-results.json" {
		return nil, fmt.Errorf("unexpected Docker probe report path")
	}
	return os.ReadFile(filepath.Join(env.Worktree, name))
}
