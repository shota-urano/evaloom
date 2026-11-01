package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
	"github.com/shota-urano/evaloom/backend/internal/safegit"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/securitytest"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

var gitDefenseCases = []string{"default-hooks", "included-hooks", "worktree-fsmonitor", "included-filters"}

type gitDefenseRuntime func(t *testing.T, f *securitytest.Fixture, common string) (probe.Sandbox, sandbox.Sandbox)

// Normal verification exercises the actual host Git, Gates, Publisher and HTTP
// reporting. The process doubles run ONLY generated fixtures. The security_docker
// variant uses the identical assertions with real Harness/Test Sandboxes.
func TestHostGitDefense(t *testing.T) {
	runGitDefense(t, func(t *testing.T, f *securitytest.Fixture, _ string) (probe.Sandbox, sandbox.Sandbox) {
		return fixtureProcessDouble{home: f.Home}, baselineRuntimeFunc(func(ctx context.Context, spec sandbox.SandboxSpec, argv []string) (sandbox.Process, error) {
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Dir, cmd.Env = spec.Worktree, []string{"PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1"}
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return &baselineCommandProcess{cmd}, nil
		})
	})
}

func runGitDefense(t *testing.T, runtime gitDefenseRuntime) {
	t.Helper()
	for _, mode := range gitDefenseCases {
		t.Run(mode, func(t *testing.T) { gitDefenseExecution(t, mode, runtime) })
	}
}

func gitDefenseExecution(t *testing.T, mode string, runtime gitDefenseRuntime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// /tmp is shared by Docker Desktop as well as available to normal tests.
	root, err := os.MkdirTemp("/tmp", "evaloom-git-defense-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := securitytest.NewFixture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	raw := func(args ...string) string {
		t.Helper()
		out, err := f.Git(ctx, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// The original repository stands for the owner's clone. All attack code and
	// attribute policy are committed before the managed clone is prepared.
	gate2Write(t, filepath.Join(f.Worktree, "harness.py"), "from git_attack import main\nmain()\n")
	gate2Write(t, filepath.Join(f.Worktree, ".gitattributes"), "app.py filter=Defense.Driver\n")
	raw("config", "evaloom.owner", "owner-original")
	raw("add", ".")
	raw("commit", "-m", "Git defense fixture")
	f.BaseSHA = raw("rev-parse", "HEAD")
	raw("push", "origin", "HEAD:refs/heads/main")
	owner := f.Repository
	ownerState := func() [2]string {
		refs, err := safegit.Output(ctx, owner, "for-each-ref", "--sort=refname", "--format=%(refname) %(objectname) %(symref)")
		if err != nil {
			t.Fatal(err)
		}
		config, err := os.ReadFile(filepath.Join(owner, ".git", "config"))
		if err != nil {
			t.Fatal(err)
		}
		return [2]string{string(refs), string(config)}
	}
	ownerBefore := ownerState()
	url := gitDefenseRemote(t, f.Remote, f.Home)
	repository, err := (worktree.Repositories{WorktreeRoot: filepath.Join(root, "managed")}).Prepare(ctx, "git-defense", url)
	if err != nil {
		t.Fatal(err)
	}
	f.Worktree = filepath.Join(root, "execution")
	const branch = "evaloom/git-defense"
	safe := func(repo string, args ...string) string {
		t.Helper()
		out, err := safegit.Output(ctx, repo, args...)
		if err != nil {
			t.Fatalf("host Git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	safe(repository, "worktree", "add", "-b", branch, f.Worktree, f.BaseSHA)
	common := safe(f.Worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if common == filepath.Join(owner, ".git") {
		t.Fatal("managed worktree shares the owner's Git area")
	}
	provider, testSandbox := runtime(t, f, common)
	if err := (securitytest.Prerequisites{Sandbox: provider}).Check(ctx); err != nil {
		t.Fatal(err)
	}
	httpFixture := newBaselineHTTPFixture(t)
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`UPDATE projects SET local_repo_path=?,git_remote_url=?,test_command='python3 test.py',test_report_path='junit.xml'`, []any{owner, url}},
		{`UPDATE worktrees SET path=?,base_sha=?,branch_name=?`, []any{f.Worktree, f.BaseSHA, branch}},
	} {
		if _, err := httpFixture.db.Exec(stmt.query, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	r, err := New(httpFixture.config)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := r.claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	refsBefore, err := f.Refs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertNoMarks := func(stage string) {
		t.Helper()
		marks, err := os.ReadDir(filepath.Join(common, "defense-marks"))
		if err != nil || len(marks) != 0 {
			t.Fatalf("%s executed an attack payload: marks=%v err=%v", stage, marks, err)
		}
	}
	var verified, pushed string
	handlers := stubStageHandlers(func(id StageID, ctx context.Context, e *Execution) error {
		switch id {
		case StagePrepareWorktree:
			e.WorktreeID, e.WorktreePath, e.BaseSHA = httpFixture.worktreeID, f.Worktree, f.BaseSHA
		case StageHarness:
			requestData, err := os.ReadFile("../adapter/testdata/harness_request.json")
			if err != nil {
				return err
			}
			var request adapter.HarnessRequest
			if err := json.Unmarshal(requestData, &request); err != nil {
				return err
			}
			request.Project.Workdir, request.Project.BaseSHA = f.Worktree, f.BaseSHA
			h := probe.New(provider)
			env := adapter.SandboxEnv{Worktree: f.Worktree, Env: map[string]string{"EVALOOM_GIT_ATTACK": mode}}
			session, err := h.Start(ctx, request, env)
			if err != nil {
				return err
			}
			defer func() { _ = h.Cancel(session) }()
			// Logs waits for completion, including Sandbox process cleanup.
			logs, err := h.Logs(session)
			if err != nil {
				return err
			}
			var diagnostic strings.Builder
			for line := range logs {
				diagnostic.WriteString(line.Text)
			}
			result, err := h.Result(session)
			if err != nil || result.Status != adapter.ResultSucceeded {
				return fmt.Errorf("probe result=%+v err=%v logs=%s", result, err, diagnostic.String())
			}
			e.HarnessResult = &result
			report, err := os.ReadFile(filepath.Join(f.Worktree, "harness-results.json"))
			if err != nil {
				return err
			}
			observations, err := probe.DecodeReport(report)
			if err != nil || len(observations) != 1 || observations[0].ID != "git-defense-installed" || observations[0].Kind != "git-config" || !*observations[0].Succeeded || observations[0].Detail != mode {
				return fmt.Errorf("attack installation report=%s err=%v", report, err)
			}
			assertNoMarks("probe installation")
			// Exercise host worktree creation/checkout and fetch with the armed
			// common area, in addition to the Gate/Publisher status calls below.
			safe(repository, "worktree", "add", "-b", "evaloom/after-probe", filepath.Join(root, "after-probe"), "HEAD")
			if _, err := (worktree.Repositories{WorktreeRoot: filepath.Join(root, "managed")}).Prepare(ctx, "git-defense", url); err != nil {
				return err
			}
			assertNoMarks("Runner worktree/checkout/fetch")
		case StagePublish:
			assertNoMarks("Runner Gates 2/3")
			refs, err := f.Refs(ctx)
			if err != nil || !reflect.DeepEqual(refs, refsBefore) {
				return fmt.Errorf("probe unexpectedly changed remote refs: %v %v", refs, err)
			}
			verified = e.VerifiedSHA
			pushed, err = publisher.Push(ctx, publisher.Target{Worktree: f.Worktree, VerifiedSHA: verified, BranchName: branch}, publisher.PushConfig{
				RepositoryURL: url, CheckCancellation: r.Heartbeat,
				RecordRemoteMismatch: func(context.Context, string, string) error { return fmt.Errorf("unexpected remote mismatch") },
			})
			assertNoMarks("Git Publisher")
			return err
		}
		return nil
	})
	control := testExecutionControl()
	control.Timeouts = Timeouts{Setup: time.Minute, Harness: time.Minute, Test: time.Minute}
	control.Reporter, control.CheckCancellation = r, r.Heartbeat
	executor, err := r.NewVerificationExecutor(handlers, control, Gate2Config{BaselineConfig: BaselineConfig{
		Sandbox: testSandbox, Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 256 << 20, Time: time.Minute}, Shell: "/bin/sh", JUnitFormat: "junit",
	}}, Gate3Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(ctx, *claim); err != nil {
		t.Fatalf("execution: %v: %v", err, unwrapStage(err))
	}
	var status string
	var gates, runs int
	if err := httpFixture.db.QueryRow(`SELECT status,(SELECT count(*) FROM gate_results WHERE execution_id=e.id AND gate IN (2,3) AND result='pass'),
		(SELECT count(*) FROM test_runs WHERE execution_id=e.id AND kind='verify' AND target_sha=e.verified_sha AND passed=2)
		FROM executions e WHERE id=?`, claim.Execution.ID).Scan(&status, &gates, &runs); err != nil || status != "succeeded" || gates != 2 || runs != 1 {
		t.Fatalf("status=%s gates=%d verify runs=%d err=%v", status, gates, runs, err)
	}
	refs, err := f.Refs(ctx)
	wantRefs := map[string]string{"refs/heads/main": f.BaseSHA, "refs/heads/" + branch: verified}
	if err != nil || verified == "" || verified == f.BaseSHA || pushed != verified || !reflect.DeepEqual(refs, wantRefs) {
		t.Fatalf("verified=%s pushed=%s refs=%v want=%v err=%v", verified, pushed, refs, wantRefs, err)
	}
	if got := ownerState(); got != ownerBefore {
		t.Fatalf("owner refs/config changed: before=%q after=%q", ownerBefore, got)
	}
	if raw("config", "--get", "evaloom.owner") != "harness-modified" || raw("rev-parse", "refs/heads/main") != verified {
		t.Fatal("probe did not mutate managed config and ref")
	}
	// Positive controls run after all safety assertions. They use the SAME
	// installed scripts and real Git operations, never merely execute a script.
	var expected []string
	switch mode {
	case "default-hooks", "included-hooks":
		raw("checkout", "--detach", "HEAD")
		raw("commit", "--allow-empty", "-m", "unsafe control")
		raw("push", url, "HEAD:refs/heads/evaloom/unsafe-control")
		expected = []string{"post-checkout", "pre-push", "reference-transaction"}
	case "worktree-fsmonitor":
		raw("status", "--porcelain")
		expected = []string{"fsmonitor"}
	case "included-filters":
		raw("worktree", "add", "--detach", filepath.Join(root, "unsafe-checkout"), "HEAD")
		file := filepath.Join(f.Worktree, "app.py")
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		gate2Write(t, file, string(body)+"\n# unsafe control\n")
		raw("add", "app.py")
		expected = []string{"clean", "smudge"}
	}
	for _, name := range expected {
		body, err := os.ReadFile(filepath.Join(common, "defense-marks", name))
		if err != nil || string(body) != "executed" {
			t.Fatalf("unprotected Git did not execute %s: %q %v", name, body, err)
		}
	}
	t.Logf("Gates 2/3 passed; pushed verified SHA; owner refs/config unchanged; unprotected Git executed %v", expected)
}

// Local receive-pack is intentionally forbidden by safegit. A loopback daemon
// serves the disposable bare remote, as in the Publisher's existing tests.
func gitDefenseRemote(t *testing.T, remote, home string) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	root := filepath.Dir(remote)
	cmd := exec.CommandContext(ctx, "git", "daemon", "--reuseaddr", "--listen=127.0.0.1", "--port="+port,
		"--export-all", "--enable=receive-pack", "--base-path="+root, root)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return "git://" + address + "/" + filepath.Base(remote)
		}
		if time.Now().After(deadline) {
			t.Fatal("実行不能: Git daemon did not listen")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
