//go:build security_docker

package securitytest

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// TestDockerTestSandboxIsolation covers spec 14 §3.3 with the fixture's actual
// setup.py and test.py, each in a fresh Test Sandbox with its own network policy.
// Selecting security_docker makes prerequisites mandatory; there are no skips.
func TestDockerTestSandboxIsolation(t *testing.T) {
	image := os.Getenv("EVALOOM_SECURITY_TEST_IMAGE")
	if image == "" {
		t.Fatal("実行不能: EVALOOM_SECURITY_TEST_IMAGE must name a prebuilt Linux image (see Dockerfile.boundary)")
	}
	runtime, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("実行不能: %v", err)
	}
	runtime, err = filepath.Abs(runtime)
	if err != nil {
		t.Fatal(err)
	}
	runtimeEnv := map[string]string{}
	for _, key := range []string{"HOME", "PATH", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		if value, ok := os.LookupEnv(key); ok {
			runtimeEnv[key] = value
		}
	}
	docker := func(args ...string) []byte { return boundaryCommand(t, runtime, runtimeEnv, args...) }
	arch := string(docker("version", "--format", "{{.Server.Arch}}"))
	if arch != "arm64" && arch != "amd64" {
		t.Fatalf("実行不能: unsupported Docker architecture %q", arch)
	}
	docker("image", "inspect", image)
	// Independent positive control: these destinations must be live from the
	// same Docker engine before a denied direct connection can prove isolation.
	// The tests never send credentials or payloads to these public TCP endpoints.
	docker("run", "--rm", "--pull", "never", "--network", "bridge", "--entrypoint", "/usr/bin/env", image,
		"-i", "PATH=/usr/local/bin:/usr/bin:/bin", "python3", "-c", `import socket, ssl, urllib.request, subprocess
subprocess.run(['git','--version'], check=True)
for host in ['registry.npmjs.org', 'github.com', '1.1.1.1']:
    with socket.create_connection((host, 443), 8):
        pass`)
	root, err := os.MkdirTemp("/tmp", "evaloom-isolation-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "proxy")
	buildCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, "../../cmd/evaloom-sandbox-proxy")
	build.Env = slices.DeleteFunc(os.Environ(), func(value string) bool {
		return strings.HasPrefix(value, "GOOS=") || strings.HasPrefix(value, "GOARCH=") || strings.HasPrefix(value, "CGO_ENABLED=")
	})
	build.Env = append(build.Env, "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("実行不能: build proxy: %v\n%s", err, out)
	}
	for _, relaxed := range []bool{false, true} {
		name := "isolated"
		if relaxed {
			name = "negative_control_allow_secret_env"
		}
		t.Run(name, func(t *testing.T) {
			runIsolationDocker(t, filepath.Join(root, name), runtime, image, binary, runtimeEnv, relaxed)
		})
	}
}

func runIsolationDocker(t *testing.T, root, runtime, image, binary string, runtimeEnv map[string]string, relaxed bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := NewFixture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	dataDir, configDir := filepath.Join(root, "data"), filepath.Join(root, "config")
	for _, dir := range []string{dataDir, configDir, filepath.Join(f.Home, "Library", "Keychains"), filepath.Join(f.Home, ".ssh")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	secret := NewCanary()
	files := map[string]string{
		"runner-token":     filepath.Join(root, "runner-token"),
		"publisher":        filepath.Join(f.Home, ".git-credentials"),
		"publisher-config": filepath.Join(f.Home, ".gitconfig"),
		"ssh-key":          filepath.Join(f.Home, ".ssh", "id_ed25519"),
		"keychain":         filepath.Join(f.Home, "Library", "Keychains", "login.keychain-db"),
		"home-canary":      filepath.Join(f.Home, "private-canary"),
		// This untracked secret belongs to the source project, outside the
		// execution worktree. Probe both its source path and worktree path.
		"project-dotenv": filepath.Join(f.Repository, ".env"),
		"logs":           filepath.Join(dataDir, "execution.log"),
		"artifacts":      filepath.Join(dataDir, "artifact.txt"),
		"model-config":   filepath.Join(configDir, "model-credentials"),
	}
	for _, path := range files {
		if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != secret {
			t.Fatalf("実行不能: host canary positive control: %v", err)
		}
	}
	files["worktree-dotenv"] = filepath.Join(f.Worktree, ".env")
	dbPath := filepath.Join(dataDir, "evaloom.db")
	control := newBoundaryControl(t, dbPath)
	// A real host management socket proves absence isn't just a misspelled
	// target. Standard Docker/containerd/Desktop sockets are probed as well.
	socketPath := filepath.Join(root, "runner.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if info, err := os.Stat(socketPath); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("実行不能: host management socket positive control: %v", err)
	}
	targets := isolationTargets{db: dbPath, files: files, worktree: f.Worktree, secret: secret,
		sockets: []string{socketPath, "/var/run/docker.sock", "/run/docker.sock", "/run/containerd/containerd.sock",
			"/run/host-services/ssh-auth.sock", "/run/host-services/backend.sock"}}
	for _, server := range control.servers {
		targets.controlURLs = append(targets.controlURLs, server.URL)
	}
	vars := map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "LC_ALL": "C.UTF-8", "PYTHONDONTWRITEBYTECODE": "1"}
	vars["SSH_AUTH_SOCK"] = socketPath
	t.Setenv("SSH_AUTH_SOCK", socketPath)
	for _, key := range []string{"EVALOOM_ISOLATION_SECRET", "RUNNER_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY"} {
		t.Setenv(key, secret)
		vars[key] = secret
	}
	var stdout, stderr bytes.Buffer
	config := sandbox.ContainerConfig{RuntimePath: runtime, Image: image, ProxyImage: image, ProxyBinary: binary, RuntimeEnv: runtimeEnv,
		AllowEnv: slices.Clone(isolationEnv), WorktreeRoot: f.Worktree, GitDirRoot: filepath.Join(f.Repository, ".git"), ConfigDirRoot: configDir,
		HostHome: f.Home, DataDir: dataDir, RunnerTokenFile: files["runner-token"], ControlTimeout: 15 * time.Second,
		Stdout: &stdout, Stderr: &stderr}
	if relaxed {
		config.AllowEnv = append(config.AllowEnv, "EVALOOM_ISOLATION_SECRET")
	}
	container, err := sandbox.NewContainer(config)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() boundarySnapshot {
		refs, err := f.Refs(ctx)
		if err != nil {
			t.Fatalf("trusted remote observation unavailable: %v", err)
		}
		return boundarySnapshot{http: int(control.accesses.Load()), audits: control.auditCount(t), refs: refs}
	}
	for _, stage := range []string{"setup", "test"} {
		if !t.Run(stage, func(t *testing.T) {
			plan, rules, allowObserved := isolationPlan(stage, targets)
			planData, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.Worktree, stage+"-plan.json"), planData, 0600); err != nil {
				t.Fatal(err)
			}
			spec := sandbox.SandboxSpec{Kind: sandbox.TestRun, Worktree: f.Worktree, GitDir: config.GitDirRoot, Env: vars,
				Network: sandbox.NetworkNone, Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 256 * 1024 * 1024, Time: 120 * time.Second}}
			if stage == "setup" {
				spec.Kind, spec.Network = sandbox.TestSetup, sandbox.NetworkSetup
				spec.AllowedDestinations = []string{isolationRegistry + ":443"}
			}
			provider := &boundaryDocker{container: container, spec: spec}
			if err := (Prerequisites{Sandbox: provider}).Check(ctx); err != nil {
				t.Fatalf("%v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
			}
			before := snapshot()
			// Reuse 3.2's real Container process adapter. Both workload failure
			// and unavailable runtime state fail before any denied-access verdict.
			out, err := provider.Run(ctx, adapter.SandboxEnv{Worktree: f.Worktree, Env: vars}, []string{"python3", stage + ".py"})
			if err != nil || out.ExitCode != 0 {
				t.Fatalf("実行不能: %s probe did not complete: exit=%d err=%v\nstdout: %s\nstderr: %s", stage, out.ExitCode, err, stdout.String(), stderr.String())
			}
			// Same stopped bind-mounted worktree used by boundaryDocker.ReadFile;
			// no host execution of the fixture is permitted here.
			data, err := os.ReadFile(filepath.Join(f.Worktree, stage+"-results.json"))
			if err != nil {
				t.Fatalf("実行不能: %s result unavailable: %v", stage, err)
			}
			after := snapshot()
			verdict := boundaryVerdict(data, rules, allowObserved, before, after)
			if relaxed {
				if verdict == nil || !strings.Contains(verdict.Error(), "environment") {
					t.Fatalf("negative control did not detect exposed secret: %v", verdict)
				}
				observations, err := probe.DecodeReport(data)
				if err != nil {
					t.Fatal(err)
				}
				for i := range observations {
					if observations[i].ID == "environment" {
						encoded, _ := json.Marshal(observations[i].Detail)
						var leaked struct {
							Names, Unexpected []string
							SecretPresent     bool
						}
						if json.Unmarshal(encoded, &leaked) != nil || !leaked.SecretPresent ||
							!slices.Contains(leaked.Names, "EVALOOM_ISOLATION_SECRET") || !slices.Contains(leaked.Unexpected, "EVALOOM_ISOLATION_SECRET") {
							t.Fatal("negative control did not observe the intentionally permitted secret")
						}
						observations[i].Detail = map[string]any{"names": allowObserved, "unexpected": []string{}, "secretPresent": false}
					}
				}
				if err := boundaryVerdict(verdictReport(t, observations), rules, allowObserved, before, after); err != nil {
					t.Fatalf("negative control has unrelated violations: %v", err)
				}
				t.Log("negative control detected intentionally exposed secret; all other observations matched")
			} else if verdict != nil {
				t.Fatal(verdict)
			}
			if stage == "test" {
				data, err := os.ReadFile(filepath.Join(f.Worktree, "junit.xml"))
				if err != nil {
					t.Fatal(err)
				}
				var report struct {
					XMLName  xml.Name `xml:"testsuite"`
					Tests    int      `xml:"tests,attr"`
					Failures int      `xml:"failures,attr"`
					Errors   int      `xml:"errors,attr"`
					Skipped  int      `xml:"skipped,attr"`
				}
				if xml.Unmarshal(data, &report) != nil || report.Tests != 2 || report.Failures != 0 || report.Errors != 0 || report.Skipped != 0 {
					t.Fatalf("fixture tests did not run successfully: %s", data)
				}
			}
			t.Logf("%s: %d observations matched; Control HTTP %d -> %d, audit_logs %d -> %d; remote refs unchanged",
				stage, len(rules), before.http, after.http, before.audits, after.audits)
		}) {
			return // Test depends on a successfully executed and verified setup.
		}
	}
}
