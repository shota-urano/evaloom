package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// The helper is reached only through the generated fake codex executable. It
// checks the actual argv/env/cwd received by the OS process, then exercises Git,
// JSONL, stderr and signals without a real Codex installation or model call.
func TestCodexCLIHelper(t *testing.T) {
	if os.Getenv("EVALOOM_CODEX_HELPER") != "1" {
		return
	}
	exit := func(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(91) }
	i := slices.Index(os.Args, "--")
	if i < 0 {
		exit("helper arguments missing")
	}
	args := os.Args[i+1:]
	if os.Getenv("HOME") == "" || os.Getenv("CODEX_HOME") == "" || os.Getenv("PERSONAL_SECRET") != "" ||
		os.Getenv("HOME") == os.Getenv("CODEX_HOME") {
		exit("private environment missing")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")); err != nil {
		os.Exit(1)
	}
	if !slices.Equal(args[:min(len(args), 2)], []string{"-c", "features.web_search_request=false"}) {
		exit("policy override missing")
	}
	args = args[2:]
	mode := os.Getenv("FAKE_MODE")
	if strings.HasPrefix(mode, "refresh-") {
		mode = strings.TrimPrefix(mode, "refresh-")
		if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), []byte(`{"token":"private-refreshed-token"}`), 0600); err != nil {
			exit("credential update failed")
		}
	}
	if slices.Equal(args, []string{"login", "status"}) {
		if mode == "config" {
			fmt.Fprintln(os.Stderr, "Error loading config.toml")
			os.Exit(1)
		}
		if mode == "preflight-cancel" {
			signal.Ignore(syscall.SIGTERM)
			cwd, _ := os.Getwd()
			_ = os.WriteFile(filepath.Join(cwd, "preflight-started"), []byte(strconv.Itoa(os.Getpid())), 0600)
			fmt.Println("preflight running")
			for {
				time.Sleep(time.Second)
			}
		}
		fmt.Fprintln(os.Stderr, "Logged in using ChatGPT")
		os.Exit(0)
	}
	cwd, _ := os.Getwd()
	want := []string{"exec", "--json", "--color", "never", "--ephemeral", "--sandbox", "danger-full-access", "--model", "test-model", "--cd", cwd, "--"}
	if len(args) != len(want)+1 || !slices.Equal(args[:len(want)], want) {
		exit(fmt.Sprintf("unexpected exec arguments: got=%q want=%q", args, want))
	}
	prompt := args[len(want)]
	if !strings.Contains(prompt, "fixed execution context") || !strings.Contains(prompt, `"acceptanceCriteria":"one commit"`) ||
		!strings.Contains(prompt, "commit the changes") || !strings.Contains(prompt, "Do not push") {
		exit("task prompt missing")
	}
	if mode == "cancel" || mode == "ignore-term" {
		if mode == "ignore-term" {
			signal.Ignore(syscall.SIGTERM)
		}
		_ = os.WriteFile(filepath.Join(cwd, "started"), []byte(strconv.Itoa(os.Getpid())), 0600)
		fmt.Println(`{"type":"turn.started"}`)
		for {
			time.Sleep(time.Second)
		}
	}
	switch mode {
	case "exit":
		fmt.Fprintln(os.Stderr, "execution diagnostic")
		os.Exit(23)
	case "signal":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	case "malformed":
		fmt.Println("not json")
		os.Exit(0)
	case "empty":
		os.Exit(0)
	case "incomplete":
		fmt.Println(`{"type":"turn.started"}`)
		os.Exit(0)
	case "failed":
		fmt.Println(`{"type":"turn.failed","error":{"message":"failed"}}`)
		os.Exit(0)
	case "negative":
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":-1,"output_tokens":0}}`)
		os.Exit(0)
	}
	if mode == "commit" {
		if err := os.WriteFile("change.txt", []byte("Codex change\n"), 0600); err != nil {
			exit(err.Error())
		}
		for _, args := range [][]string{{"add", "change.txt"}, {"commit", "--quiet", "-m", "fake Codex commit"}} {
			out, err := exec.Command("git", args...).CombinedOutput()
			if err != nil {
				exit(string(out))
			}
		}
	}
	fmt.Fprintln(os.Stderr, "stderr without newline")
	fmt.Println(`{"type":"thread.started","thread_id":"native-thread"}`)
	fmt.Println(`{"type":"future.event","extra":"forward compatible"}`)
	if mode != "no-summary" {
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"final summary"}}`)
	}
	if mode == "no-usage" {
		fmt.Print(`{"type":"turn.completed"}`)
	} else {
		fmt.Print(`{"type":"turn.completed","usage":{"input_tokens":17,"cached_input_tokens":5,"output_tokens":9}}`)
	}
	os.Exit(0)
}

type localProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func (p *localProcess) Wait() error           { <-p.done; return p.err }
func (p *localProcess) Stop() error           { return syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM) }
func (p *localProcess) Kill() error           { return syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL) }
func (p *localProcess) ExitCode() (int, bool) { <-p.done; return p.cmd.ProcessState.ExitCode(), true }

type fixture struct {
	h         adapter.Adapter
	request   adapter.HarnessRequest
	env       adapter.SandboxEnv
	config    Config
	settings  *harnessenv.Environment
	store     string
	processes []*localProcess
	mu        sync.Mutex
}

func newFixture(t *testing.T, mode string) *fixture {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git(repo, "init", "--quiet")
	git(repo, "commit", "--quiet", "--allow-empty", "-m", "base")
	base := git(repo, "rev-parse", "HEAD")
	git(repo, "worktree", "add", "--quiet", "-b", "test-codex", worktree)
	auth := `{"fake":true}`
	settings, err := harnessenv.Create(context.Background(), harnessenv.Config{Root: root, CommitSHA: base, ConfigVariable: "CODEX_HOME", Files: []gitcontext.File{{Path: "config.toml", Content: auth, ContentHash: fmt.Sprintf("%x", sha256.Sum256([]byte(auth))), SourceCommitSHA: base}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := settings.Close(); err != nil {
			t.Error(err)
		}
	})
	// Subscription credentials are kept outside the pinned configuration.
	store := filepath.Join(root, "stored-auth.json")
	if err := os.WriteFile(store, []byte(auth), 0600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "private-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(root, "codex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(binary, "'", "'\\''")+"' -test.run='^TestCodexCLIHelper$' -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: store, settings: settings, env: adapter.SandboxEnv{Worktree: worktree, ConfigDir: settings.ConfigDir(), Env: settings.Env()}, request: adapter.HarnessRequest{
		ContractVersion: adapter.ContractVersion, ExecutionID: "execution", Task: adapter.RequestTask{Title: "task", AcceptanceCriteria: "one commit"},
		Project: adapter.RequestProject{Workdir: worktree, BaseSHA: base}, Profile: adapter.RequestProfile{HarnessID: "harness", ModelID: "test-model", Definition: json.RawMessage(`{"toolConfiguration":{"allowedTools":["shell"]},"executionConfiguration":{"adapter":"codex","args":[]}}`)},
		Context: adapter.RequestContext{Instructions: "fixed execution context", RevisionIDs: []string{}},
	}}
	f.config = Config{ResolveAuthentication: func(adapter.HarnessRequest) (string, error) { return store, nil }, ResolveConfig: func(adapter.SandboxEnv) (*harnessenv.Environment, error) { return settings, nil },
		CancelGrace: 30 * time.Millisecond, Configure: func(_ context.Context, req adapter.HarnessRequest) ([]string, error) {
			if req.Profile.HarnessID != "harness" || !strings.Contains(string(req.Profile.Definition), "shell") {
				return nil, errors.New("policy unavailable")
			}
			return []string{"features.web_search_request=false"}, nil
		}, Launch: func(ctx context.Context, env adapter.SandboxEnv, args []string, out, errOut io.Writer) (sandbox.Process, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if (args[0] != "codex" && args[0] != "/bin/sh") || env.Worktree != worktree ||
				env.ConfigDir == settings.ConfigDir() || env.Env["CODEX_HOME"] != env.ConfigDir {
				return nil, errors.New("scope mismatch")
			}
			cmd := exec.Command(fake, args[1:]...)
			if args[0] == "/bin/sh" {
				cmd = exec.Command(args[0], args[1:]...)
			}
			cmd.Dir, cmd.Stdout, cmd.Stderr = env.Worktree, out, errOut
			cmd.Env = []string{"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + env.ConfigDir, "EVALOOM_CODEX_HELPER=1", "FAKE_MODE=" + mode, "GORACE=atexit_sleep_ms=0",
				"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
			if mode == "missing-cli" {
				cmd.Env = append(cmd.Env, "PATH=/nonexistent-evaloom-test-path")
			}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				return nil, &harnesscontext.ConfigError{Err: err}
			}
			p := &localProcess{cmd: cmd, done: make(chan struct{})}
			f.mu.Lock()
			f.processes = append(f.processes, p)
			f.mu.Unlock()
			go func() { p.err = cmd.Wait(); close(p.done) }()
			return p, nil
		}}
	f.h = New(f.config)
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, p := range f.processes {
			select {
			case <-p.done:
			default:
				_ = p.Kill()
				<-p.done
			}
		}
	})
	return f
}

func runResult(t *testing.T, f *fixture) (adapter.SessionID, adapter.HarnessResult, []adapter.LogLine) {
	t.Helper()
	id, result, logs := runResultAllowReplacement(t, f)
	assertWorkingCopiesRemoved(t, f)
	return id, result, logs
}

// Replacement tests assert cleanup by inode/path themselves: an unrelated
// object inserted at the old name must not be deleted by Environment.Close.
func runResultAllowReplacement(t *testing.T, f *fixture) (adapter.SessionID, adapter.HarnessResult, []adapter.LogLine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := f.h.Start(ctx, f.request, f.env)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := f.h.Logs(id)
	if err != nil {
		t.Fatal(err)
	}
	var logs []adapter.LogLine
	for line := range lines {
		logs = append(logs, line)
	}
	r, err := f.h.Result(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	return id, r, logs
}

func assertWorkingCopiesRemoved(t *testing.T, f *fixture) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(f.settings.ConfigDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "harness-") && entry.Name() != filepath.Base(f.settings.ConfigDir()) {
			t.Fatalf("working configuration survived Result: %s", entry.Name())
		}
	}
}

func TestCodexNonInteractiveCommit(t *testing.T) {
	t.Setenv("PERSONAL_SECRET", "must not inherit")
	f := newFixture(t, "commit")
	var registry adapter.Registry
	if err := registry.Register("codex", f.h); err != nil {
		t.Fatal(err)
	}
	f.h, _ = registry.Lookup("codex")
	id, r, logs := runResult(t, f)
	if r.Status != adapter.ResultSucceeded || r.ErrorClass != nil || r.SessionID != id || r.Summary != "final summary" ||
		r.Usage.InputTokens == nil || *r.Usage.InputTokens != 17 || r.Usage.OutputTokens == nil || *r.Usage.OutputTokens != 9 {
		t.Fatalf("result=%+v logs=%+v", r, logs)
	}
	status, err := f.h.Status(id)
	if err != nil || status != adapter.SessionSucceeded {
		t.Fatalf("status=%s err=%v", status, err)
	}
	out, err := exec.Command("git", "-C", f.env.Worktree, "rev-list", "--count", f.request.Project.BaseSHA+"..HEAD").Output()
	if err != nil || strings.TrimSpace(string(out)) != "1" {
		t.Fatalf("commits=%s err=%v", out, err)
	}
	if r.HeadSHA != "" || len(r.Commits) != 0 || r.DiffArtifactID != "" {
		t.Fatalf("adapter must leave Git fields to Runner: %+v", r)
	}
	if len(logs) < 5 || !slices.ContainsFunc(logs, func(l adapter.LogLine) bool { return l.Stream == "stderr" && l.Text == "stderr without newline" }) {
		t.Fatalf("logs=%+v", logs)
	}
	if err := f.h.Cancel(id); err != nil {
		t.Fatal(err)
	}
	after, _ := f.h.Result(id)
	if after.Status != adapter.ResultSucceeded {
		t.Fatal("Cancel changed finished result")
	}
	caps := f.h.Capabilities()
	if !caps.Streaming || !caps.TokenUsage || caps.Resume || !slices.Equal(caps.NativeInstructionFiles, []string{"AGENTS.md"}) {
		t.Fatalf("capabilities=%+v", caps)
	}
}

func TestCodexErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		class adapter.ErrorClass
	}{
		{"config", adapter.ErrorConfig}, {"missing-cli", adapter.ErrorConfig}, {"exit", adapter.ErrorExecutionFailed}, {"signal", adapter.ErrorExecutionFailed},
		{"malformed", adapter.ErrorExecutionFailed}, {"empty", adapter.ErrorExecutionFailed}, {"incomplete", adapter.ErrorExecutionFailed},
		{"failed", adapter.ErrorExecutionFailed}, {"negative", adapter.ErrorExecutionFailed},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newFixture(t, tc.mode)
			_, r, _ := runResult(t, f)
			if r.Status != adapter.ResultFailed || r.ErrorClass == nil || *r.ErrorClass != tc.class {
				t.Fatalf("result=%+v", r)
			}
		})
	}
}

func TestCodexConfigurationFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing_cli", "missing_auth", "missing_policy", "missing_config_home", "bad_profile_args", "bad_adapter", "policy_failure", "no_launcher", "no_grace", "sandbox_failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, "success")
			want := adapter.ErrorConfig
			switch mode {
			case "missing_cli":
				f.config.Launch = func(context.Context, adapter.SandboxEnv, []string, io.Writer, io.Writer) (sandbox.Process, error) {
					cmd := exec.Command(filepath.Join(t.TempDir(), "codex"))
					err := cmd.Start()
					return nil, &harnesscontext.ConfigError{Err: err}
				}
			case "missing_auth":
				if err := os.Remove(f.store); err != nil {
					t.Fatal(err)
				}
			case "missing_policy":
				f.config.Configure = nil
			case "missing_config_home":
				f.env.Env = map[string]string{}
			case "bad_profile_args":
				f.request.Profile.Definition = json.RawMessage(`{"executionConfiguration":{"args":["--dangerously-bypass-approvals-and-sandbox"]}}`)
			case "bad_adapter":
				f.request.Profile.Definition = json.RawMessage(`{"executionConfiguration":{"adapter":"claude_code"}}`)
			case "policy_failure":
				f.config.Configure = func(context.Context, adapter.HarnessRequest) ([]string, error) {
					return nil, errors.New("policy unavailable")
				}
			case "no_launcher":
				f.config.Launch = nil
			case "no_grace":
				f.config.CancelGrace = 0
			case "sandbox_failure":
				want = adapter.ErrorSandbox
				f.config.Launch = func(context.Context, adapter.SandboxEnv, []string, io.Writer, io.Writer) (sandbox.Process, error) {
					return nil, &sandbox.Error{Op: "unavailable"}
				}
			}
			f.h = New(f.config)
			_, r, _ := runResult(t, f)
			if r.Status != adapter.ResultFailed || r.ErrorClass == nil || *r.ErrorClass != want {
				t.Fatalf("result=%+v", r)
			}
		})
	}
}

func TestCodexCancellationAndTimeout(t *testing.T) {
	for _, mode := range []string{"cancel", "ignore-term", "deadline", "request_timeout", "preflight-cancel"} {
		t.Run(mode, func(t *testing.T) {
			fakeMode := "ignore-term"
			if mode == "cancel" {
				fakeMode = "cancel"
			}
			if mode == "preflight-cancel" {
				fakeMode = mode
			}
			f := newFixture(t, fakeMode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if mode == "request_timeout" {
				f.request.Limits.TimeoutSec = 1
			}
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			}
			id, err := f.h.Start(ctx, f.request, f.env)
			if err != nil {
				t.Fatal(err)
			}
			lines, _ := f.h.Logs(id)
			// A streamed event proves exec has started before cancellation.
			for line := range lines {
				if line.Stream == "stdout" && (strings.Contains(line.Text, "turn.started") || line.Text == "preflight running") {
					break
				}
			}
			status, err := f.h.Status(id)
			if err != nil || status != adapter.SessionRunning {
				t.Fatalf("status=%s err=%v", status, err)
			}
			if mode == "cancel" || mode == "ignore-term" || mode == "preflight-cancel" {
				if err := f.h.Cancel(id); err != nil {
					t.Fatal(err)
				}
			}
			for range lines {
			}
			r, err := f.h.Result(id)
			if err != nil {
				t.Fatal(err)
			}
			want := adapter.ErrorCancelled
			if mode == "deadline" || mode == "request_timeout" {
				want = adapter.ErrorTimeout
			}
			if r.ErrorClass == nil || *r.ErrorClass != want || r.Status != adapter.ResultFailed {
				t.Fatalf("result=%+v", r)
			}
			pidFile := filepath.Join(f.env.Worktree, "started")
			if mode == "preflight-cancel" {
				pidFile = filepath.Join(f.env.Worktree, "preflight-started")
			}
			pidBytes, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, _ := strconv.Atoi(string(pidBytes))
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("process %d still exists: %v", pid, err)
			}
			if _, err := os.Stat(f.env.Worktree); err != nil {
				t.Fatal("worktree not retained")
			}
			assertWorkingCopiesRemoved(t, f)
		})
	}
}

func TestCodexOptionalSummaryAndUsage(t *testing.T) {
	for _, mode := range []string{"no-summary", "no-usage"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, mode)
			_, r, _ := runResult(t, f)
			if r.Status != adapter.ResultSucceeded {
				t.Fatalf("result=%+v", r)
			}
			if mode == "no-summary" && r.Summary != "" {
				t.Fatal(r.Summary)
			}
			if mode == "no-usage" && (r.Usage.InputTokens != nil || r.Usage.OutputTokens != nil) {
				t.Fatal(r.Usage)
			}
		})
	}
}

func TestCodexUnknownSessionAndInvalidRequest(t *testing.T) {
	h := New(Config{})
	if _, err := h.Status("missing"); err == nil {
		t.Fatal("Status accepted unknown session")
	}
	if _, err := h.Result("missing"); err == nil {
		t.Fatal("Result accepted unknown session")
	}
	if _, err := h.Logs("missing"); err == nil {
		t.Fatal("Logs accepted unknown session")
	}
	if err := h.Cancel("missing"); err == nil {
		t.Fatal("Cancel accepted unknown session")
	}
	f := newFixture(t, "success")
	f.env.Worktree = "/different"
	if _, err := f.h.Start(context.Background(), f.request, f.env); err == nil {
		t.Fatal("Start accepted wrong worktree")
	}
}
