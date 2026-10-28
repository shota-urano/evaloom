//go:build darwin || linux

package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

// This helper runs only in subprocesses. The root waits for its child; both
// belong to one process group, as a sandbox launcher and harness would.
func TestCancellationProcessHelper(t *testing.T) {
	mode := os.Getenv("EVALOOM_STOP_TEST")
	if mode == "" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	if strings.HasPrefix(mode, "root-") {
		child := exec.Command(os.Args[0], "-test.run=^TestCancellationProcessHelper$")
		child.Env = append(os.Environ(), "EVALOOM_STOP_TEST="+strings.TrimPrefix(mode, "root-"))
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Run(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	term := make(chan os.Signal, 1)
	if mode == "graceful" {
		signal.Notify(term, syscall.SIGTERM)
	}
	fmt.Println(os.Getpid()) // Ready only after the signal policy is installed.
	select {
	case <-term:
	case <-time.After(time.Hour):
	}
	os.Exit(0)
}

// Test-only process launcher. Production must supply a real isolated sandbox;
// the supervisor itself never launches an unrestricted host command.
type testSandboxProcess struct {
	cmd          *exec.Cmd
	child        int
	done         chan struct{}
	err          error
	stops, kills atomic.Int32
	cancels      atomic.Int32
	wrongSession atomic.Bool
}

func startTestSandbox(t *testing.T, worktree, mode string) *testSandboxProcess {
	t.Helper()
	process := &testSandboxProcess{done: make(chan struct{})}
	process.cmd = exec.Command(os.Args[0], "-test.run=^TestCancellationProcessHelper$")
	process.cmd.Env = append(os.Environ(), "EVALOOM_STOP_TEST=root-"+mode, "GORACE=atexit_sleep_ms=0")
	process.cmd.Dir = worktree
	process.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := process.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := process.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		process.err = process.cmd.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		select {
		case <-process.done:
			if process.child > 0 && syscall.Kill(process.child, 0) == syscall.ESRCH {
				return
			}
		default:
		}
		syscall.Kill(-process.cmd.Process.Pid, syscall.SIGKILL)
		await(t, process.done)
	})
	ready := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(output)
		line, _ := reader.ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	process.child, err = strconv.Atoi(await(t, ready))
	if err != nil {
		t.Fatalf("helper did not start its child: %v", err)
	}
	for _, pid := range []int{process.cmd.Process.Pid, process.child} {
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("process %d was not alive before cancellation: %v", pid, err)
		}
	}
	return process
}

func (process *testSandboxProcess) Wait() error {
	<-process.done
	// The direct child has been reaped. Ensure its descendant has also exited
	// before allowing result reporting, including the SIGKILL escalation path.
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(process.child, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			return fmt.Errorf("sandbox descendant %d is still alive", process.child)
		}
		time.Sleep(time.Millisecond)
	}
	return process.err
}

func (process *testSandboxProcess) Stop() error {
	process.stops.Add(1)
	return syscall.Kill(-process.cmd.Process.Pid, syscall.SIGTERM)
}

func (process *testSandboxProcess) Kill() error {
	process.kills.Add(1)
	return syscall.Kill(-process.cmd.Process.Pid, syscall.SIGKILL)
}

func (process *testSandboxProcess) Cancel(id adapter.SessionID) error {
	if id != "test-session" {
		process.wrongSession.Store(true)
	}
	process.cancels.Add(1)
	return nil // Sandbox Stop performs the actual signal in this fixture.
}

func retainedGitWorktree(t *testing.T) (repo, worktree string) {
	t.Helper()
	root := t.TempDir()
	repo, worktree = filepath.Join(root, "repo"), filepath.Join(root, "worktree")
	gitTestCommand(t, "init", repo)
	gitTestCommand(t, "-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "initial")
	gitTestCommand(t, "-C", repo, "worktree", "add", "-b", "evaloom/task-001-stop", worktree)
	if err := os.WriteFile(filepath.Join(worktree, "unfinished.txt"), []byte("keep this work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return repo, worktree
}

func gitTestCommand(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestHarnessTimeoutStopsProcessesReportsTimedOutAndRetainsWorktree(t *testing.T) {
	testControlledProcess(t, StageHarness, false, "stubborn")
}

func TestHeartbeatCancellationStopsProcessesReportsCancelledAndRetainsWorktree(t *testing.T) {
	for _, mode := range []string{"graceful", "stubborn"} {
		t.Run(mode, func(t *testing.T) { testControlledProcess(t, StageHarness, true, mode) })
	}
}

func TestSetupAndTestTimeoutStopProcessesAndRetainWorktree(t *testing.T) {
	for _, stage := range []StageID{StageSetup, StageTestBaseline, StageGate2} {
		t.Run(string(stage), func(t *testing.T) { testControlledProcess(t, stage, false, "stubborn") })
	}
}

func testControlledProcess(t *testing.T, target StageID, cancelled bool, mode string) {
	t.Helper()
	f := newLogControlPlane(t)
	repo, worktree := retainedGitWorktree(t)
	worktreeID := ulid.Make().String()
	base := gitTestCommand(t, "-C", worktree, "rev-parse", "HEAD")
	_, err := f.db.Exec(`INSERT INTO worktrees (id,project_id,task_id,runner_id,path,branch_name,base_sha,status,setup_status,copied_files,created_at)
		VALUES (?,'project','task-001','mac-mini',?,'evaloom/task-001-stop',?,'active','succeeded','[]','now')`, worktreeID, worktree, base)
	if err == nil {
		_, err = f.db.Exec(`UPDATE executions SET status='claimed' WHERE id=?`, logExecution)
	}
	if err != nil {
		t.Fatal(err)
	}
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	process.pollInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deliveryCtx, stopDelivery := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopDelivery()
	if err := process.RegisterExecution(logExecution, cancel); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { process.ForgetExecution(logExecution) })
	started := make(chan *testSandboxProcess, 1)
	var called []StageID
	limit := 300 * time.Millisecond
	if cancelled {
		limit = 10 * time.Second
	}
	executor, err := NewExecutorWithControl(stubStageHandlers(func(stage StageID, ctx context.Context, execution *Execution) error {
		called = append(called, stage)
		if stage == StagePrepareWorktree {
			execution.WorktreeID = worktreeID
		}
		if stage != target {
			return nil
		}
		sandbox := startTestSandbox(t, worktree, mode)
		started <- sandbox
		if stage == StageHarness {
			return execution.WaitHarness(ctx, sandbox, sandbox, "test-session")
		}
		return execution.WaitProcess(ctx, sandbox)
	}), ExecutionControl{
		Timeouts: Timeouts{Setup: limit, Harness: limit, Test: limit}, CancelGrace: 80 * time.Millisecond,
		Reporter: process, ReportContext: deliveryCtx,
		CheckCancellation: process.Heartbeat,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := Claim{}
	claim.Execution.ID = logExecution
	done := make(chan error, 1)
	go func() { done <- executor.Execute(ctx, claim) }()
	sandbox := await(t, started)
	if cancelled {
		// The production heartbeat endpoint returns this request until Runner
		// reports cancelled. Repeat heartbeat to check cancellation idempotency.
		if _, err := f.db.Exec(`UPDATE executions SET cancel_requested_at='now' WHERE id=?`, logExecution); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := process.Heartbeat(deliveryCtx); err != nil {
				t.Fatal(err)
			}
		}
	}
	err = await(t, done)
	wantErr, wantStatus, wantClass := context.DeadlineExceeded, "timed_out", "timeout"
	if cancelled {
		wantErr, wantStatus, wantClass = context.Canceled, "cancelled", "cancelled"
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("termination error=%v; want %v", err, wantErr)
	}
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage.ID != target || called[len(called)-1] != target {
		t.Fatalf("continued beyond interrupted stage: %v, %v", called, err)
	}
	for _, pid := range []int{sandbox.cmd.Process.Pid, sandbox.child} {
		if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
			t.Errorf("process %d survived terminal report: %v", pid, err)
		}
	}
	if sandbox.stops.Load() != 1 || sandbox.wrongSession.Load() {
		t.Fatalf("sandbox stop/session mismatch: stops=%d wrongSession=%v", sandbox.stops.Load(), sandbox.wrongSession.Load())
	}
	wantCancels := int32(0)
	if target == StageHarness {
		wantCancels = 1
	}
	if sandbox.cancels.Load() != wantCancels {
		t.Fatalf("Adapter Cancel calls=%d; want %d", sandbox.cancels.Load(), wantCancels)
	}
	wantKills := int32(0)
	if mode == "stubborn" {
		wantKills = 1
		if !sandbox.cmd.ProcessState.Sys().(syscall.WaitStatus).Signaled() || sandbox.cmd.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("stubborn process was not killed: %v", sandbox.cmd.ProcessState)
		}
	}
	if sandbox.kills.Load() != wantKills {
		t.Fatalf("Kill calls=%d; want %d", sandbox.kills.Load(), wantKills)
	}
	var status, class, taskStatus, retained string
	if err := f.db.QueryRow(`SELECT e.status,e.error_class,t.status,w.status FROM executions e
		JOIN tasks t ON t.id=e.task_id JOIN worktrees w ON w.id=e.worktree_id WHERE e.id=?`, logExecution).
		Scan(&status, &class, &taskStatus, &retained); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || class != wantClass || taskStatus != wantStatus || retained != "retained" {
		t.Fatalf("stored result: execution=%s error_class=%s task=%s worktree=%s", status, class, taskStatus, retained)
	}
	content, err := os.ReadFile(filepath.Join(worktree, "unfinished.txt"))
	if err != nil || string(content) != "keep this work\n" {
		t.Fatalf("worktree data was not retained: %q %v", content, err)
	}
	if got := gitTestCommand(t, "-C", worktree, "branch", "--show-current"); got != "evaloom/task-001-stop" {
		t.Fatalf("worktree branch changed: %s", got)
	}
	if !strings.Contains(gitTestCommand(t, "-C", repo, "worktree", "list", "--porcelain"), worktree) {
		t.Fatal("worktree registration was removed")
	}
	var terminalReports int
	if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE execution_id=? AND type='status_changed'
		AND json_extract(payload,'$.to')=?`, logExecution, wantStatus).Scan(&terminalReports); err != nil || terminalReports != 1 {
		t.Fatalf("terminal reports=%d err=%v", terminalReports, err)
	}
}

var _ SandboxProcess = (*testSandboxProcess)(nil)
var _ SessionCanceller = (*testSandboxProcess)(nil)
