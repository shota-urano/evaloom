package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

const readCanary = "EVALOOM_C2X7_READ_INSTALLATION_TOKEN_679fcba1"
const readURL = "git@github.com:owner/repo.git"

type readAPIRoute struct{ endpoint string }

func (r readAPIRoute) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	if copy.URL.Host != "api.github.com" {
		return nil, errors.New("unexpected API host")
	}
	copy.URL.Scheme, copy.URL.Host = "http", strings.TrimPrefix(r.endpoint, "http://")
	return http.DefaultTransport.RoundTrip(copy)
}

func readCredentials(t *testing.T, scenario string) (*publisher.Reader, *atomic.Int64) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	calls := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/app/installations/176795851/access_tokens" ||
			!strings.HasPrefix(r.Header.Get("Authorization"), "Bearer eyJ") {
			t.Error("wrong App token request")
		}
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Repositories, []string{"repo"}) ||
			!reflect.DeepEqual(body.Permissions, map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"}) {
			t.Error("read token request must contain exactly the three read permissions and one repository")
		}
		if scenario == "uninstalled" || scenario == "denied" {
			status := 404
			if scenario == "denied" {
				status = 403
			}
			http.Error(w, readCanary, status)
			return
		}
		if scenario == "redirect" {
			w.Header().Set("Location", "/leak?token="+readCanary)
			w.WriteHeader(307)
			return
		}
		permissions := map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"}
		repo := "owner/repo"
		expires := time.Now().Add(time.Hour)
		switch scenario {
		case "write_token":
			permissions["contents"] = "write"
		case "extra_permission":
			permissions["checks"] = "read"
		case "missing_permission":
			delete(permissions, "pull_requests")
		case "wrong_repo":
			repo = "other/repo"
		case "expired":
			expires = time.Now().Add(-time.Hour)
		case "malformed":
			w.WriteHeader(201)
			_, _ = io.WriteString(w, readCanary)
			return
		}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": readCanary, "expires_at": expires,
			"permissions": permissions, "repositories": []map[string]string{{"full_name": repo}}})
	}))
	t.Cleanup(server.Close)
	reader, err := publisher.NewGitHubAppReader(&http.Client{Transport: readAPIRoute{server.URL}}, readURL,
		func(context.Context) ([]byte, error) {
			if scenario == "key" {
				return nil, errors.New(readCanary)
			}
			return keyPEM, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return reader, calls
}

// Only the test executable changes github.com's destination and TLS CA. The
// real Git still authenticates with the child-only header and verifies TLS.
func TestReadGitHelper(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 {
		return
	}
	realGit, endpoint, ca, record := os.Args[i+1], os.Args[i+2], os.Args[i+3], os.Args[i+4]
	args, env := os.Args[i+5:], os.Environ()
	if os.Getenv("GIT_CONFIG_KEY_0") == "http.https://github.com/owner/repo.git.extraHeader" {
		expected := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+readCanary))
		if os.Getenv("GIT_CONFIG_VALUE_0") != expected || os.Getenv("GIT_CONFIG_COUNT") != "1" ||
			os.Getenv("GIT_ALLOW_PROTOCOL") != "https" || os.Getenv("GIT_TRACE_CURL") != "" || os.Getenv("GITHUB_TOKEN") != "" ||
			strings.Contains(strings.Join(args, "\n"), readCanary) || strings.Contains(strings.Join(args, "\n"), expected) ||
			!slices.Contains(args, "http.followRedirects=false") || !slices.Contains(args, "http.sslVerify=true") {
			os.Exit(80)
		}
		operation := ""
		for j, arg := range args {
			if arg == "https://github.com/owner/repo.git" {
				args[j] = endpoint + "/remote.git"
			}
			if arg == "clone" || arg == "fetch" || arg == "ls-remote" {
				operation = arg
			}
		}
		for j, value := range env {
			if strings.HasPrefix(value, "GIT_CONFIG_KEY_0=") {
				env[j] = "GIT_CONFIG_KEY_0=http." + endpoint + "/remote.git.extraHeader"
			}
		}
		args = append([]string{"-c", "http.sslCAInfo=" + ca}, args...)
		file, err := os.OpenFile(record, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(81)
		}
		_, _ = fmt.Fprintln(file, operation)
		_ = file.Close()
	}
	cmd := exec.Command(realGit, args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		os.Exit(82)
	}
	os.Exit(0)
}

func readGitScript(t *testing.T, path, testName string, args ...string) {
	t.Helper()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexport GORACE=atexit_sleep_ms=0\nexec " + quote(self) + " -test.run=^" + testName + "$ --"
	for _, arg := range args {
		script += " " + quote(arg)
	}
	if err := os.WriteFile(path, []byte(script+" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func readGitServer(t *testing.T, private, realGit, scenario string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	calls := new(atomic.Int64)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, token, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || token != readCanary {
			t.Error("Git HTTP request did not authenticate with the read token")
			http.Error(w, "unauthenticated", 401)
			return
		}
		calls.Add(1)
		if scenario == "git_rejected" {
			http.Error(w, readCanary, 403)
			return
		}
		if scenario == "git_redirect" {
			w.Header().Set("Location", "/leak?token="+readCanary)
			w.WriteHeader(307)
			return
		}
		cmd := exec.Command(realGit, "http-backend")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
			"GIT_PROJECT_ROOT=" + private, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=reader", "REQUEST_METHOD=" + r.Method,
			"PATH_INFO=" + r.URL.Path, "QUERY_STRING=" + r.URL.RawQuery, "CONTENT_TYPE=" + r.Header.Get("Content-Type")}
		cmd.Stdin = r.Body
		out, err := cmd.Output()
		if err != nil {
			t.Error("Git HTTP backend failed")
			w.WriteHeader(500)
			return
		}
		body := bufio.NewReader(bytes.NewReader(out))
		headers, err := textproto.NewReader(body).ReadMIMEHeader()
		if err != nil {
			t.Error(err)
			return
		}
		status := 200
		for name, values := range headers {
			if name == "Status" {
				status, _ = strconv.Atoi(strings.Fields(values[0])[0])
			} else {
				w.Header()[name] = values
			}
		}
		w.WriteHeader(status)
		_, _ = io.Copy(w, body)
	}))
	t.Cleanup(server.Close)
	return server, calls
}

func assertReadSecretsAbsent(t *testing.T, data []byte) {
	t.Helper()
	for _, value := range []string{readCanary, base64.StdEncoding.EncodeToString([]byte("x-access-token:" + readCanary))} {
		if bytes.Contains(data, []byte(value)) {
			t.Fatal("read credential found in persisted or Sandbox data")
		}
	}
}

func TestReadTokenPermissionsAndAuthenticatedStage3Isolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	f := newLogControlPlane(t)
	if _, err := f.db.Exec(`UPDATE projects SET git_remote_url=?; UPDATE executions SET status='preparing'`, readURL); err != nil {
		t.Fatal(err)
	}
	p, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	control := testExecutionControl()
	control.Reporter = p
	e := &Execution{control: &control, status: model.ExecutionPreparing}
	e.Claim.Execution.ID, e.Claim.Execution.TaskID, e.Claim.Execution.Mode = logExecution, "task-001", "continue"
	e.Claim.Execution.Seq, e.Claim.Task.ShortName = 1, "read"
	e.Claim.Project = model.Project{ID: "project", GitRemoteURL: readURL, BaseBranch: "main"}
	private := t.TempDir()
	seed, remote := filepath.Join(private, "seed"), filepath.Join(private, "remote.git")
	gate2Git(t, private, "init", "--initial-branch=main", seed)
	gate2Write(t, filepath.Join(seed, "source"), "initial")
	gate2Git(t, seed, "add", ".")
	gate2Git(t, seed, "commit", "-m", "initial")
	gate2Git(t, private, "init", "--bare", "--initial-branch=main", remote)
	gate2Git(t, remote, "fetch", seed, "HEAD:refs/heads/main")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	server, gitCalls := readGitServer(t, private, realGit, "success")
	ca, record := filepath.Join(private, "ca.pem"), filepath.Join(private, "git-calls")
	gate2Write(t, ca, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})))
	readGitScript(t, filepath.Join(private, "git"), "TestReadGitHelper", realGit, server.URL, ca, record)
	t.Setenv("PATH", private+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_TRACE_CURL", filepath.Join(f.config.WorktreeRoot, "trace"))
	t.Setenv("GITHUB_TOKEN", "ambient-token-must-not-be-used")
	beforeEnv := os.Environ()
	reader, tokenCalls := readCredentials(t, "success")
	repositories, err := e.PrepareReadRepository(ctx, worktree.Repositories{WorktreeRoot: f.config.WorktreeRoot}, reader)
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(f.config.WorktreeRoot, ".evaloom-repositories", "project")
	// A fetch must import a newer commit than clone, while hostile HTTP,
	// include, helper and hook settings must never see credentials.
	gate2Write(t, filepath.Join(seed, "source"), "new remote commit")
	gate2Git(t, seed, "commit", "-am", "advance")
	gate2Git(t, remote, "fetch", seed, "HEAD:refs/heads/main")
	want := gate2Git(t, remote, "rev-parse", "refs/heads/main")
	gate2Git(t, clone, "config", "http.https://github.com/.proxy", "http://127.0.0.1:1")
	gate2Git(t, clone, "config", "http.cookieFile", filepath.Join(f.config.WorktreeRoot, "cookies"))
	gate2Git(t, clone, "config", "http.saveCookies", "true")
	gate2Git(t, clone, "config", "credential.helper", "!env > '"+filepath.Join(f.config.WorktreeRoot, "helper-env")+"'")
	include := filepath.Join(clone, ".git", "hostile.config")
	gate2Write(t, include, "[http \"https://github.com/\"]\n\tsslVerify = false\n\tproxy = http://127.0.0.1:1\n")
	gate2Git(t, clone, "config", "include.path", include)
	hooks := filepath.Join(clone, ".git", "hostile-hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(hooks, "post-checkout")
	gate2Write(t, hook, "#!/bin/sh\nenv > '"+filepath.Join(f.config.WorktreeRoot, "hook-env")+"'\n")
	if err := os.Chmod(hook, 0700); err != nil {
		t.Fatal(err)
	}
	gate2Git(t, clone, "config", "core.hooksPath", hooks)
	beforeConfig, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	// Also exercise Prepare's wildcard fetch into an existing clone, as used
	// by later executions. It must import the new remote tracking ref.
	if _, err := repositories.Prepare(ctx, "project", readURL); err != nil || gate2Git(t, clone, "rev-parse", "refs/remotes/origin/main") != want {
		t.Fatalf("existing clone refresh failed: %v", err)
	}
	if err := e.RecordRemoteBase(ctx, repositories); err != nil || e.BaseSHA != want {
		t.Fatalf("Base Branch fetch did not use the current remote: %v", err)
	}
	if err := e.CreateWorktree(ctx, repositories, 0, pathAuditFunc(func(context.Context, string) error { return errors.New("unexpected audit") })); err != nil {
		t.Fatal(err)
	}
	if gate2Git(t, e.WorktreePath, "rev-parse", "HEAD") != want || tokenCalls.Load() != 1 || gitCalls.Load() < 5 {
		t.Fatal("clone/fetch/ls-remote did not use one stage-scoped read token")
	}
	data, err := os.ReadFile(record)
	if err != nil || string(data) != "clone\nfetch\nfetch\nls-remote\n" {
		t.Fatalf("authenticated Git commands=%q err=%v", data, err)
	}
	afterConfig, err := os.ReadFile(filepath.Join(clone, ".git", "config"))
	if err != nil || !bytes.Equal(beforeConfig, afterConfig) || !reflect.DeepEqual(beforeEnv, os.Environ()) {
		t.Fatal("read authentication changed persisted config or parent environment")
	}
	for _, name := range []string{"trace", "cookies", "helper-env", "hook-env"} {
		if _, err := os.Stat(filepath.Join(f.config.WorktreeRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("Git executed an untrusted logger/helper/hook: %s", name)
		}
	}
	// NewLog receives the actual token exclusion registered during issuance.
	// Deliberately echo both the raw token and Git's encoded authentication.
	log, err := p.NewLog(logExecution, "test", nil, e.LogSecrets()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Finish(false) })
	if err := log.Read(strings.NewReader(readCanary + "\n" + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+readCanary)) + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := log.Finish(false); err != nil {
		t.Fatal(err)
	}
	if err := log.Deliver(ctx); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(log.Path())
	if err != nil || string(data) != "[REDACTED]\n[REDACTED]\n" {
		t.Fatal("read token was not excluded from Runner log")
	}
	probeReadSandbox(t, e.WorktreePath, clone, private)
	for _, root := range []string{f.config.WorktreeRoot, f.config.DataDir} {
		for _, contents := range baseOwnerSnapshot(t, root) {
			assertReadSecretsAbsent(t, []byte(contents))
		}
	}
	rows, err := f.db.Query(`SELECT payload FROM execution_events WHERE type='log' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		assertReadSecretsAbsent(t, []byte(payload))
		if !strings.Contains(payload, "[REDACTED]") {
			t.Fatal("Runner event did not contain redacted text")
		}
		count++
	}
	if rows.Err() != nil || count != 2 {
		t.Fatal("expected two redacted Runner events")
	}
}

// Records actual Container argv/environment; this is not an OS isolation test.
func TestReadSandboxHelper(t *testing.T) {
	record := os.Getenv("EVALOOM_READ_SANDBOX_RECORD")
	if record == "" {
		return
	}
	i := slices.Index(os.Args, "--")
	args := os.Args[i+1:]
	file, err := os.OpenFile(record, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(83)
	}
	_ = json.NewEncoder(file).Encode(struct{ Args, Env []string }{args, os.Environ()})
	_ = file.Close()
	if args[0] == "image" {
		fmt.Print(`{"Os":"linux","Config":{}}`)
	} else if args[1] == "inspect" {
		fmt.Print(`{"Status":"exited","Running":false,"ExitCode":0}`)
	}
	os.Exit(0)
}

func probeReadSandbox(t *testing.T, worktree, clone, private string) {
	t.Helper()
	record, runtime := filepath.Join(private, "sandbox-record"), filepath.Join(private, "sandbox-runtime")
	config := filepath.Join(private, "sandbox-config")
	if err := os.Mkdir(config, 0700); err != nil {
		t.Fatal(err)
	}
	readGitScript(t, runtime, "TestReadSandboxHelper")
	container, err := sandbox.NewContainer(sandbox.ContainerConfig{RuntimePath: runtime, Image: "probe", ControlTimeout: 5 * time.Second,
		RuntimeEnv: map[string]string{"EVALOOM_READ_SANDBOX_RECORD": record}, AllowEnv: []string{"PROBE"},
		WorktreeRoot: worktree, GitDirRoot: clone, ConfigDirRoot: config, HostHome: private,
		DataDir: filepath.Join(private, "data"), RunnerTokenFile: filepath.Join(private, "runner-token"), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []sandbox.SandboxKind{sandbox.Harness, sandbox.TestSetup, sandbox.TestRun} {
		mount := ""
		if kind == sandbox.Harness {
			mount = config
		}
		process, err := container.Run(context.Background(), sandbox.SandboxSpec{Kind: kind, Worktree: worktree, GitDir: clone,
			ConfigDir: mount, Env: map[string]string{"PROBE": "read-isolation"},
			Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 1 << 20, Time: 10 * time.Second}}, []string{"env"})
		if err != nil {
			t.Fatal(err)
		}
		if err := process.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(record)
	if err != nil || !bytes.Contains(data, []byte("PROBE=read-isolation")) {
		t.Fatal("Sandbox environment was not observed")
	}
	assertReadSecretsAbsent(t, data)
}

func TestStage3ReadTokenFailuresReportConfigError(t *testing.T) {
	for _, scenario := range []string{"key", "denied", "uninstalled", "redirect", "write_token", "extra_permission", "missing_permission", "wrong_repo", "expired", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLogControlPlane(t)
			if _, err := f.db.Exec(`UPDATE executions SET status='preparing'`); err != nil {
				t.Fatal(err)
			}
			p, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			control := testExecutionControl()
			control.Reporter = p
			e := &Execution{control: &control, status: model.ExecutionPreparing}
			e.Claim.Execution.ID, e.Claim.Execution.Mode = logExecution, "fresh"
			e.Claim.Project = model.Project{ID: "project", GitRemoteURL: readURL, BaseBranch: "main"}
			credentials, calls := readCredentials(t, scenario)
			_, err = e.PrepareReadRepository(context.Background(), worktree.Repositories{WorktreeRoot: f.config.WorktreeRoot}, credentials)
			if err == nil || e.BaseSHA != "" || e.WorktreePath != "" || calls.Load() > 1 {
				t.Fatal("authentication failure was accepted, retried or prepared a worktree")
			}
			assertReadSecretsAbsent(t, []byte(err.Error()))
			var status, class, message string
			if err := f.db.QueryRow(`SELECT status,error_class,error_message FROM executions WHERE id=?`, logExecution).Scan(&status, &class, &message); err != nil || status != "failed" || class != "config_error" {
				t.Fatalf("stage3 authentication failure status=%s class=%s err=%v", status, class, err)
			}
			assertReadSecretsAbsent(t, []byte(message))
			if _, err := os.Stat(f.config.WorktreeRoot); !os.IsNotExist(err) {
				t.Fatal("token failure touched the worktree root")
			}
		})
	}
}

func TestStage3ReadGitFailuresDoNotExposeServerResponse(t *testing.T) {
	for _, scenario := range []string{"git_rejected", "git_redirect"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLogControlPlane(t)
			if _, err := f.db.Exec(`UPDATE executions SET status='preparing'`); err != nil {
				t.Fatal(err)
			}
			p, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			control := testExecutionControl()
			control.Reporter = p
			e := &Execution{control: &control, status: model.ExecutionPreparing}
			e.Claim.Execution.ID, e.Claim.Execution.Mode = logExecution, "fresh"
			e.Claim.Project = model.Project{ID: "project", GitRemoteURL: readURL}
			private := t.TempDir()
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			server, gitCalls := readGitServer(t, private, realGit, scenario)
			ca := filepath.Join(private, "ca.pem")
			gate2Write(t, ca, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})))
			readGitScript(t, filepath.Join(private, "git"), "TestReadGitHelper", realGit, server.URL, ca, filepath.Join(private, "git-record"))
			t.Setenv("PATH", private+string(os.PathListSeparator)+os.Getenv("PATH"))
			credentials, calls := readCredentials(t, "success")
			_, err = e.PrepareReadRepository(context.Background(), worktree.Repositories{WorktreeRoot: f.config.WorktreeRoot}, credentials)
			if err == nil || calls.Load() != 1 || gitCalls.Load() != 1 {
				t.Fatal("Git rejection was accepted, retried or followed a redirect")
			}
			assertReadSecretsAbsent(t, []byte(err.Error()))
			var status, class, message string
			if err := f.db.QueryRow(`SELECT status,error_class,error_message FROM executions WHERE id=?`, logExecution).Scan(&status, &class, &message); err != nil || status != "failed" || class != "config_error" {
				t.Fatalf("Git failure status=%s class=%s err=%v", status, class, err)
			}
			assertReadSecretsAbsent(t, []byte(message))
			for _, content := range baseOwnerSnapshot(t, f.config.WorktreeRoot) {
				assertReadSecretsAbsent(t, []byte(content))
			}
		})
	}
}
