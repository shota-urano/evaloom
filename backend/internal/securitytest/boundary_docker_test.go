package securitytest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// This glue exists only in acceptance tests. Check runs a real isolated
// workload; neither adapter execution nor file reads can fall back to the host.
type boundaryDocker struct {
	container *sandbox.Container
	spec      sandbox.SandboxSpec
}

func (s *boundaryDocker) Check(ctx context.Context) error {
	_, err := s.Run(ctx, adapter.SandboxEnv{Worktree: s.spec.Worktree, ConfigDir: s.spec.ConfigDir, Env: s.spec.Env},
		[]string{"python3", "-c", `import socket, ssl, urllib.request, subprocess; subprocess.run(['git','--version'], check=True)`})
	return err
}

func (s *boundaryDocker) Run(ctx context.Context, env adapter.SandboxEnv, command []string) (probe.Output, error) {
	if env.Worktree != s.spec.Worktree || env.ConfigDir != s.spec.ConfigDir {
		return probe.Output{}, errors.New("probe paths differ from Docker mounts")
	}
	spec := s.spec
	spec.Env = env.Env
	p, err := s.container.Run(ctx, spec, command)
	if err != nil {
		return probe.Output{}, err
	}
	err = p.Wait()
	code, known := p.ExitCode()
	var exit *sandbox.ExitError
	if !known || err != nil && !errors.As(err, &exit) {
		return probe.Output{}, fmt.Errorf("Docker workload unavailable: %w", err)
	}
	return probe.Output{ExitCode: code}, err
}

func (s *boundaryDocker) ReadFile(ctx context.Context, env adapter.SandboxEnv, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if env.Worktree != s.spec.Worktree || name != "harness-results.json" {
		return nil, errors.New("unexpected report path")
	}
	// Docker bind mounts this exact host worktree; the workload has stopped.
	return os.ReadFile(filepath.Join(env.Worktree, name))
}

func boundaryCommand(t *testing.T, runtime string, env map[string]string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtime, args...)
	cmd.Env = []string{}
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("実行不能: docker %v: %v\n%s", args, err, out)
	}
	return bytes.TrimSpace(out)
}

type boundaryControl struct {
	db       *sql.DB
	servers  []*httptest.Server
	accesses atomic.Int32
}

func newBoundaryControl(t *testing.T, dbPath string) *boundaryControl {
	t.Helper()
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	c := &boundaryControl{db: db}
	t.Cleanup(func() { _ = db.Close() })
	v4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("実行不能: IPv4 Control API listener: %v", err)
	}
	t.Cleanup(func() { _ = v4.Close() })
	v6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatalf("実行不能: IPv6 Control API listener: %v", err)
	}
	t.Cleanup(func() { _ = v6.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth, err := api.NewOwnerAuth(db, logger, v4.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	handler := api.NewHandler(db, logger, auth)
	for _, listener := range []net.Listener{v4, v6} {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.accesses.Add(1) // Record all requests, including authentication rejects.
			handler.ServeHTTP(w, r)
		}))
		_ = server.Listener.Close()
		server.Listener = listener
		server.Start()
		c.servers = append(c.servers, server)
		t.Cleanup(server.Close)
	}
	// A broken listener/auth/audit writer must not make blocked probes pass.
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	for _, server := range c.servers {
		for _, endpoint := range []struct{ method, path string }{{"GET", "/api/projects"}, {"POST", "/api/runners/boundary/claim"}} {
			req, err := http.NewRequest(endpoint.method, server.URL+endpoint.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
				t.Fatalf("Control API positive control status=%d", response.StatusCode)
			}
		}
	}
	if c.accesses.Load() != 4 || c.auditCount(t) != 4 {
		t.Fatal("Control API positive controls did not produce HTTP and audit evidence")
	}
	return c
}

func (c *boundaryControl) auditCount(t *testing.T) int {
	t.Helper()
	var count int
	if err := c.db.QueryRow("SELECT count(*) FROM audit_logs").Scan(&count); err != nil {
		t.Fatalf("trusted audit observation unavailable: %v", err)
	}
	return count
}

// TestDockerPermissionBoundaries covers spec 14 §3.2 through the probe Adapter
// and real Harness Sandbox, including the destination-filtering sidecars.
func TestDockerPermissionBoundaries(t *testing.T) {
	if os.Getenv("EVALOOM_SECURITY_DOCKER_TEST") != "1" {
		t.Skip("set EVALOOM_SECURITY_DOCKER_TEST=1 to enable real permission-boundary tests")
	}
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
	docker("run", "--rm", "--pull", "never", "--network", "none", "--entrypoint", "/usr/bin/env", image,
		"-i", "PATH=/usr/local/bin:/usr/bin:/bin", "python3", "-c", `import socket, ssl, urllib.request, subprocess; subprocess.run(['git','--version'],check=True)`)
	// Keep Unix socket paths short on macOS, and canonicalize /tmp for mounts.
	root, err := os.MkdirTemp("/tmp", "evaloom-boundary-")
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
			runBoundaryDocker(t, root, runtime, image, binary, runtimeEnv, relaxed)
		})
	}
}

func runBoundaryDocker(t *testing.T, root, runtime, image, binary string, runtimeEnv map[string]string, relaxed bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	caseRoot := filepath.Join(root, t.Name()[strings.LastIndex(t.Name(), "/")+1:])
	if err := os.Mkdir(caseRoot, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := NewFixture(ctx, caseRoot)
	if err != nil {
		t.Fatal(err)
	}
	dataDir, configDir := filepath.Join(caseRoot, "data"), filepath.Join(caseRoot, "config")
	for _, dir := range []string{dataDir, configDir, filepath.Join(f.Home, "Library", "Keychains")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	secret := NewCanary()
	paths := map[string]string{
		"runner-token": filepath.Join(caseRoot, "runner-token"),
		"logs":         filepath.Join(dataDir, "execution.log"), "artifacts": filepath.Join(dataDir, "artifact.txt"),
		"publisher":             filepath.Join(f.Home, ".git-credentials"),
		"publisher-config":      filepath.Join(f.Home, ".gitconfig"),
		"keychain":              filepath.Join(f.Home, "Library", "Keychains", "login.keychain-db"),
		"personal-model-config": filepath.Join(f.Home, ".model-credentials"),
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
			t.Fatal(err)
		}
	}
	modelConfig := filepath.Join(configDir, "dedicated-model-credentials")
	if err := os.WriteFile(modelConfig, []byte(NewCanary()), 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dataDir, "evaloom.db")
	control := newBoundaryControl(t, dbPath)
	github := NewGitHub("fixture/project", secret)
	t.Cleanup(github.Close)
	// Verify the fake GitHub listener too, then baseline its trusted record.
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get(github.Server.URL + "/repos/fixture/project/pulls")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || len(github.Accesses()) != 1 {
		t.Fatal("fake GitHub positive control failed")
	}
	agentPath := filepath.Join(root, "agent-"+strconv.FormatBool(relaxed)+".sock")
	agent, err := net.Listen("unix", agentPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	var agentAccesses atomic.Int32
	go func() {
		for {
			conn, err := agent.Accept()
			if err != nil {
				return
			}
			agentAccesses.Add(1)
			_, _ = conn.Write([]byte("test-agent"))
			_ = conn.Close()
		}
	}()
	conn, err := net.Dial("unix", agentPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(conn)
	_ = conn.Close()
	if agentAccesses.Load() != 1 {
		t.Fatal("SSH agent positive control failed")
	}
	allowEnv := []string{"PATH", "LC_ALL", "PYTHONDONTWRITEBYTECODE", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_TERMINAL_PROMPT",
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"}
	vars := map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "LC_ALL": "C.UTF-8", "PYTHONDONTWRITEBYTECODE": "1",
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_TERMINAL_PROMPT": "0",
		"GIT_AUTHOR_NAME": "Evaloom Test", "GIT_AUTHOR_EMAIL": "evaloom-test@example.invalid",
		"GIT_COMMITTER_NAME": "Evaloom Test", "GIT_COMMITTER_EMAIL": "evaloom-test@example.invalid"}
	for _, key := range []string{"EVALOOM_BOUNDARY_SECRET", "GITHUB_TOKEN", "RUNNER_TOKEN", "OPENAI_API_KEY"} {
		t.Setenv(key, secret) // Detect accidental host inheritance as well as Env filtering.
		vars[key] = secret
	}
	vars["SSH_AUTH_SOCK"] = agentPath
	t.Setenv("SSH_AUTH_SOCK", agentPath)
	var stdout, stderr bytes.Buffer
	config := sandbox.ContainerConfig{RuntimePath: runtime, Image: image, ProxyImage: image, ProxyBinary: binary, RuntimeEnv: runtimeEnv,
		AllowEnv: slices.Clone(allowEnv), WorktreeRoot: f.Worktree, GitDirRoot: filepath.Join(f.Repository, ".git"), ConfigDirRoot: configDir,
		HostHome: f.Home, DataDir: dataDir, RunnerTokenFile: paths["runner-token"], ControlTimeout: 15 * time.Second,
		Stdout: &stdout, Stderr: &stderr}
	if relaxed {
		// Intentionally wrong trusted configuration, restricted to a dummy secret.
		// The expected allowlist/plan/oracle remain identical to the isolated case.
		config.AllowEnv = append(config.AllowEnv, "EVALOOM_BOUNDARY_SECRET")
	}
	container, err := sandbox.NewContainer(config)
	if err != nil {
		t.Fatal(err)
	}
	provider := &boundaryDocker{container: container, spec: sandbox.SandboxSpec{
		Kind: sandbox.Harness, Worktree: f.Worktree, GitDir: config.GitDirRoot, ConfigDir: configDir, Env: vars,
		Network: sandbox.NetworkHarness, AllowedDestinations: []string{"example.com:443"},
		Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 256 * 1024 * 1024, Time: 120 * time.Second}}}
	if err := (Prerequisites{Sandbox: provider}).Check(ctx); err != nil {
		t.Fatalf("%v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	var plan []map[string]any
	var rules []boundaryRule
	add := func(id, kind string, allowed bool, item map[string]any) {
		item["id"], item["kind"] = id, kind
		plan = append(plan, item)
		rules = append(rules, boundaryRule{id, kind, allowed})
	}
	for i, server := range control.servers {
		family := []string{"v4", "v6"}[i]
		for _, endpoint := range []struct{ id, method, path string }{{"owner", "GET", "/api/projects"}, {"runner", "POST", "/api/runners/boundary/claim"}} {
			add("control-"+endpoint.id+"-"+family, "http", false,
				map[string]any{"url": server.URL + endpoint.path, "method": endpoint.method, "timeoutSec": 1})
		}
		// Probe Docker Desktop aliases and literal loopback through the proxy too.
		_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
		portNumber, _ := strconv.Atoi(port)
		for _, host := range []string{"127.0.0.1", "::1", "host.docker.internal", "gateway.docker.internal"} {
			add("control-proxy-"+family+"-"+host, "proxy_connect", false, map[string]any{"host": host, "port": portNumber, "timeoutSec": 2})
			if host == "host.docker.internal" || host == "gateway.docker.internal" {
				add("control-direct-"+family+"-"+host, "http", false,
					map[string]any{"url": "http://" + net.JoinHostPort(host, port) + "/api/projects", "timeoutSec": 1})
			}
		}
	}
	add("db-read", "read", false, map[string]any{"path": dbPath})
	add("db-write", "write", false, map[string]any{"path": dbPath, "value": "test-owned-boundary-write"})
	for _, id := range []string{"logs", "artifacts", "runner-token", "publisher", "publisher-config", "keychain", "personal-model-config"} {
		add(id, "read", false, map[string]any{"path": paths[id]})
	}
	add("ssh-agent-file", "exists", false, map[string]any{"path": agentPath})
	add("ssh-agent", "command", false, map[string]any{"argv": []string{"python3", "-c",
		`import socket,sys; s=socket.socket(socket.AF_UNIX); s.settimeout(1); s.connect(sys.argv[1])`, agentPath}, "timeoutSec": 2})
	allowObserved := append(slices.Clone(allowEnv), "HOME", "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy")
	add("environment", "env", true, map[string]any{"allowlist": allowObserved, "secrets": []string{secret}})
	add("git-push", "git_push", false, map[string]any{"remote": f.Remote, "refspec": "HEAD:refs/heads/boundary-escape", "timeoutSec": 3})
	add("github-direct", "tcp", false, map[string]any{"host": "github.com", "port": 443, "timeoutSec": 2})
	add("github-proxy", "proxy_connect", false, map[string]any{"host": "github.com", "port": 443, "timeoutSec": 2})
	add("fake-github", "http", false, map[string]any{"url": github.Server.URL + "/repos/fixture/project/pulls", "timeoutSec": 1})
	// Q8 chose option 2: dedicated model credentials are intentionally visible.
	add("dedicated-model-config", "read", true, map[string]any{"path": modelConfig})
	add("worktree-positive", "read", true, map[string]any{"path": filepath.Join(f.Worktree, "app.py")})
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Worktree, "harness-plan.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := func() boundarySnapshot {
		refs, err := f.Refs(ctx)
		if err != nil {
			t.Fatalf("trusted remote observation unavailable: %v", err)
		}
		return boundarySnapshot{http: int(control.accesses.Load()), audits: control.auditCount(t), github: len(github.Accesses()), agent: int(agentAccesses.Load()), refs: refs}
	}
	before := snapshot()
	requestData, err := os.ReadFile("../adapter/testdata/harness_request.json")
	if err != nil {
		t.Fatal(err)
	}
	var request adapter.HarnessRequest
	if err := json.Unmarshal(requestData, &request); err != nil {
		t.Fatal(err)
	}
	request.Project.Workdir = f.Worktree
	request.Project.BaseSHA = f.BaseSHA
	harness := probe.New(provider)
	env := adapter.SandboxEnv{Worktree: f.Worktree, ConfigDir: configDir, Env: vars}
	id, err := harness.Start(ctx, request, env)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = harness.Cancel(id) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := harness.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		if status != adapter.SessionRunning {
			break
		}
		select {
		case <-ctx.Done():
			_ = harness.Cancel(id)
			_, _ = harness.Logs(id) // Wait for Docker cleanup before deleting mounts.
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	result, err := harness.Result(id)
	if err != nil || result.Status != adapter.ResultSucceeded {
		t.Fatalf("probe Adapter did not complete: result=%+v err=%v\nstdout: %s\nstderr: %s", result, err, stdout.String(), stderr.String())
	}
	data, err = provider.ReadFile(ctx, env, "harness-results.json")
	if err != nil {
		t.Fatal(err)
	}
	after := snapshot()
	verdict := boundaryVerdict(data, rules, allowObserved, before, after)
	if relaxed {
		if verdict == nil || !strings.Contains(verdict.Error(), "environment") {
			t.Fatalf("negative control did not detect exposed secret: %v", verdict)
		}
		// Require every other observation to pass, so an unrelated failure cannot
		// masquerade as a successful negative control.
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
					!slices.Contains(leaked.Names, "EVALOOM_BOUNDARY_SECRET") || !slices.Contains(leaked.Unexpected, "EVALOOM_BOUNDARY_SECRET") {
					t.Fatal("negative control did not observe the intentionally permitted secret variable")
				}
				observations[i].Detail = map[string]any{"names": allowObserved, "unexpected": []string{}, "secretPresent": false}
			}
		}
		if err := boundaryVerdict(verdictReport(t, observations), rules, allowObserved, before, after); err != nil {
			t.Fatalf("negative control has unrelated violations: %v", err)
		}
		t.Log("negative control detected intentionally exposed environment secret")
	} else if verdict != nil {
		t.Fatal(verdict)
	} else {
		t.Logf("all %d observations matched; Control HTTP/audit, GitHub/agent access records and remote refs unchanged", len(rules))
	}
}
