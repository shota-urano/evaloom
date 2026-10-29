package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This acceptance test is intentionally outside deterministic make verify.
// Enabling it makes missing Docker/images/build tools a failure, never a skip.
func TestDockerNetworkIsolation(t *testing.T) {
	if os.Getenv("EVALOOM_SANDBOX_DOCKER_TEST") != "1" {
		t.Skip("set EVALOOM_SANDBOX_DOCKER_TEST=1 to run real Docker isolation probes")
	}
	image := os.Getenv("EVALOOM_SANDBOX_TEST_IMAGE")
	if image == "" {
		t.Fatal("EVALOOM_SANDBOX_TEST_IMAGE is required (preinstalled Linux image with /bin/busybox and /usr/bin/env)")
	}
	runtimePath, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
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
	docker := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, runtimePath, args...)
		for key, value := range runtimeEnv {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return bytes.TrimSpace(out)
	}
	arch := string(docker("version", "--format", "{{.Server.Arch}}"))
	if arch != "arm64" && arch != "amd64" {
		t.Fatalf("unsupported test build architecture %q", arch)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"home", "data", "worktree", "git", "config", "trusted"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(root, "trusted", "proxy")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/evaloom-sandbox-proxy")
	build.Env = os.Environ()
	for _, key := range []string{"GOOS", "GOARCH", "CGO_ENABLED"} {
		build.Env = slices.DeleteFunc(build.Env, func(value string) bool { return strings.HasPrefix(value, key+"=") })
	}
	build.Env = append(build.Env, "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build proxy: %v\n%s", err, out)
	}
	var accesses atomic.Int32
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accesses.Add(1)
			_ = conn.Close()
		}
	}()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	// Prove the host canary is listening, then reset its access counter before
	// probes. Its accept loop is a second observation, separate from exit codes.
	canary, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = canary.Read(make([]byte, 1))
	_ = canary.Close()
	accesses.Store(0)
	hosts := []string{"127.0.0.1", "::1", "host.docker.internal", "gateway.docker.internal"}
	var ipam []struct{ Gateway string }
	if err := json.Unmarshal(docker("network", "inspect", "bridge", "--format", "{{json .IPAM.Config}}"), &ipam); err != nil {
		t.Fatal(err)
	}
	for _, entry := range ipam {
		if entry.Gateway != "" {
			hosts = append(hosts, entry.Gateway)
		}
	}
	// Resolve Desktop's host aliases in an ordinary bridge container as well.
	// Even if DNS itself is unreachable from network-none, probe these literal
	// addresses so a DNS failure cannot conceal an IP route to the host.
	for _, alias := range []string{"host.docker.internal", "gateway.docker.internal"} {
		out := docker("run", "--rm", "--pull", "never", "--network", "bridge", "--entrypoint", "/bin/sh", image,
			"-c", "/bin/busybox nslookup \"$1\" 2>/dev/null || true", "probe", alias)
		for _, field := range strings.Fields(string(out)) {
			if ip := net.ParseIP(field); ip != nil {
				hosts = append(hosts, ip.String())
			}
		}
	}
	config := ContainerConfig{RuntimePath: runtimePath, Image: image, ProxyImage: image, ProxyBinary: binary,
		RuntimeEnv: runtimeEnv, AllowEnv: []string{"PATH"}, ControlTimeout: 15 * time.Second,
		WorktreeRoot: filepath.Join(root, "worktree"), GitDirRoot: filepath.Join(root, "git"), ConfigDirRoot: filepath.Join(root, "config"),
		HostHome: filepath.Join(root, "home"), DataDir: filepath.Join(root, "data"), RunnerTokenFile: filepath.Join(root, "token")}
	for _, kind := range []SandboxKind{Harness, TestSetup, TestRun} {
		t.Run(string(kind), func(t *testing.T) {
			var output bytes.Buffer
			config.Stdout, config.Stderr = &output, &output
			container, err := NewContainer(config)
			if err != nil {
				t.Fatal(err)
			}
			spec := SandboxSpec{Kind: kind, Worktree: config.WorktreeRoot, GitDir: config.GitDirRoot,
				Env: map[string]string{"PATH": "/usr/bin:/bin"}, Limits: ResourceLimits{CPU: 1, MemoryBytes: 128 * 1024 * 1024, Time: 90 * time.Second}}
			if kind == Harness {
				spec.ConfigDir = config.ConfigDirRoot
				spec.Network = NetworkHarness
				spec.AllowedDestinations = []string{"example.com:443"}
			}
			if kind == TestSetup {
				spec.Network = NetworkSetup
				spec.AllowedDestinations = []string{"example.com:443"}
			}
			if kind == TestRun {
				spec.Network = NetworkNone
			}
			// First prove nc works against a listener within this namespace.
			// Then attempt raw TCP and CONNECT for all forbidden destinations.
			script := `set -eu
/bin/busybox nc -l -p 18081 >/dev/null &
probe_pid=$!
/bin/busybox sleep 0.2
/bin/busybox nc -w 1 127.0.0.1 18081 </dev/null
wait "$probe_pid"
shift
while [ "$#" -gt 0 ]; do
  host="$1"; port="$2"; shift 2
  if /bin/busybox nc -w 1 "$host" "$port" </dev/null >/dev/null 2>&1; then
    echo "UNEXPECTED direct connection $host:$port"; exit 41
  fi
  if [ -n "${HTTPS_PROXY:-}" ]; then
    authority="$host:$port"
    case "$host" in *:*) authority="[$host]:$port";; esac
    response=$({ /bin/busybox printf 'CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n' "$authority" "$authority"; /bin/busybox sleep 1; } | /bin/busybox nc -w 2 127.0.0.1 18080 || true)
    case "$response" in *"403 Forbidden"*) ;; *) echo "proxy failed to explicitly deny $host:$port: $response"; exit 42;; esac
  fi
  echo "blocked $host:$port"
done
`
			args := []string{"/bin/sh", "-c", script, "probe", "destinations"}
			for _, host := range hosts {
				args = append(args, host, port)
			}
			// GitHub is forbidden for harness and also outside setup's list.
			args = append(args, "github.com", "443", "1.1.1.1", "443")
			p, err := container.Run(context.Background(), spec, args)
			if err != nil {
				t.Fatalf("Run: %v\n%s", err, output.String())
			}
			if err := p.Wait(); err != nil {
				t.Fatalf("Wait: %v\n%s", err, output.String())
			}
			if got := strings.Count(output.String(), "blocked "); got != len(hosts)+2 {
				t.Fatalf("incomplete probes (%d):\n%s", got, output.String())
			}
			t.Log(output.String())
		})
	}
	if accesses.Load() != 0 {
		t.Fatalf("host Control API canary accepted %d sandbox connections", accesses.Load())
	}
	// Optional positive Internet probe. Set a known reachable public HTTPS
	// host:port; it must be explicitly provided, never a production default.
	if destination := os.Getenv("EVALOOM_SANDBOX_TEST_ALLOWED_DESTINATION"); destination != "" {
		t.Run("allowed_destination", func(t *testing.T) {
			var output bytes.Buffer
			config.Stdout, config.Stderr = &output, &output
			container, err := NewContainer(config)
			if err != nil {
				t.Fatal(err)
			}
			spec := SandboxSpec{Kind: TestSetup, Worktree: config.WorktreeRoot, Network: NetworkSetup, AllowedDestinations: []string{destination},
				Limits: ResourceLimits{CPU: 1, MemoryBytes: 128 * 1024 * 1024, Time: 60 * time.Second}}
			script := `response=$({ /bin/busybox printf 'CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n' "$1" "$1"; /bin/busybox sleep 5; } | /bin/busybox nc -w 5 127.0.0.1 18080); echo "$response"; case "$response" in *"200 Connection Established"*) exit 0;; *) exit 1;; esac`
			p, err := container.Run(context.Background(), spec, []string{"/bin/sh", "-c", script, "probe", destination})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Wait(); err != nil {
				t.Fatalf("allowed destination: %v\n%s", err, output.String())
			}
			t.Log(fmt.Sprintf("allowed %s: %s", destination, output.String()))
		})
	}
}
