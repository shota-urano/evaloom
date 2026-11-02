package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

type setupProcess struct {
	wait   func() error
	stop   func() error
	kill   func() error
	code   *int
	waited atomic.Bool
}

func (p *setupProcess) Wait() error { err := p.wait(); p.waited.Store(true); return err }
func (p *setupProcess) Stop() error {
	if p.stop != nil {
		return p.stop()
	}
	return nil
}
func (p *setupProcess) Kill() error {
	if p.kill != nil {
		return p.kill()
	}
	return nil
}
func (p *setupProcess) ExitCode() (int, bool) {
	if p.waited.Load() && p.code != nil {
		return *p.code, true
	}
	return 0, false
}

// Setup measurement/status delivery, terminal reports and logs use authenticated
// HTTP and SQLite. Only the Sandbox launch boundary is replaced in setup tests.
func newSetupExecution(t *testing.T) (*logControlPlane, *Execution, *Runner) {
	t.Helper()
	f, e, r, _ := newCopyExecution(t)
	if err := e.CopyWorktreeFiles(context.Background(), r, store.WorktreeCopyStore{DB: f.db}); err != nil {
		t.Fatal(err)
	}
	p, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	p.pollInterval = time.Millisecond
	e.control.Reporter = p
	return f, e, p
}

func setupCommand(t *testing.T, f *logControlPlane, e *Execution, command string) {
	t.Helper()
	e.Claim.Project.SetupCommand = &command
	if _, err := f.db.Exec(`UPDATE projects SET setup_command=? WHERE id='project'`, command); err != nil {
		t.Fatal(err)
	}
}

func setupStage(p *Runner) SetupStage {
	return SetupStage{NewLog: func(id string) (*Log, error) {
		log, err := p.NewLog(id, "setup", nil)
		if log != nil {
			log.retryInterval = time.Millisecond
		}
		return log, err
	}}
}

func readSetupRun(t *testing.T, f *logControlPlane, e *Execution) (model.TestRun, string) {
	t.Helper()
	var run model.TestRun
	var started, finished, status string
	if err := f.db.QueryRow(`SELECT r.id,r.execution_id,r.kind,r.target_sha,r.command,r.exit_code,r.timed_out,r.started_at,r.finished_at,w.setup_status
		FROM test_runs r JOIN executions e ON e.id=r.execution_id JOIN worktrees w ON w.id=e.worktree_id WHERE r.execution_id=? AND r.kind='setup'`, e.Claim.Execution.ID).
		Scan(&run.ID, &run.ExecutionID, &run.Kind, &run.TargetSHA, &run.Command, &run.ExitCode, &run.TimedOut, &started, &finished, &status); err != nil {
		t.Fatal(err)
	}
	var err error
	if run.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
		t.Fatal(err)
	}
	if run.FinishedAt, err = time.Parse(time.RFC3339Nano, finished); err != nil {
		t.Fatal(err)
	}
	if run.TargetSHA != e.BaseSHA || run.Command != *e.Claim.Project.SetupCommand || run.FinishedAt.Before(run.StartedAt) {
		t.Fatalf("incorrect setup measurement: %+v", run)
	}
	return run, status
}

func assertSetupTerminal(t *testing.T, f *logControlPlane, e *Execution, status, class string) {
	t.Helper()
	var saved, errorClass, task, worktree string
	if err := f.db.QueryRow(`SELECT e.status,e.error_class,t.status,w.status FROM executions e JOIN tasks t ON t.id=e.task_id
		JOIN worktrees w ON w.id=e.worktree_id WHERE e.id=?`, e.Claim.Execution.ID).Scan(&saved, &errorClass, &task, &worktree); err != nil {
		t.Fatal(err)
	}
	if saved != status || errorClass != class || task != status || worktree != "retained" {
		t.Fatalf("execution=%s class=%s task=%s worktree=%s", saved, errorClass, task, worktree)
	}
	if _, err := os.Stat(e.WorktreePath); err != nil {
		t.Fatalf("worktree not retained: %v", err)
	}
}

func TestSetupUsesOnlyTestSandboxAndPersistsFailedRunAndLog(t *testing.T) {
	f, e, p := newSetupExecution(t)
	canary := filepath.Join(t.TempDir(), "host-executed")
	command := fmt.Sprintf("printf host > %q", canary)
	setupCommand(t, f, e, command)
	stage := setupStage(p)
	var reports atomic.Int32
	var firstBody []byte
	p.client.Transport = usageRetryTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/result") {
			return http.DefaultTransport.RoundTrip(r)
		}
		if r.Header.Get("Authorization") != "Bearer "+p.token {
			t.Error("setup result lacks Runner authentication")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		var result ExecutionResult
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, err
		}
		response, err := http.DefaultTransport.RoundTrip(r)
		if result.SetupStatus != "" {
			attempt := reports.Add(1)
			if attempt == 1 {
				firstBody = body
				if err != nil || response.StatusCode != 204 {
					t.Errorf("initial setup report: %v %v", response, err)
				} else {
					response.Body.Close()
					return nil, errors.New("lost setup ACK after commit")
				}
			} else if attempt == 2 && string(firstBody) != string(body) {
				t.Error("setup retry changed the measurement")
			}
		}
		return response, err
	})
	calls := 0
	process := &setupProcess{code: new(23)}
	stage.Prepare = func(_ context.Context, request SetupRequest, stdout, stderr io.Writer) (sandbox.Sandbox, sandbox.SandboxSpec, []string, error) {
		if request.Command != command || request.TargetSHA != e.BaseSHA || request.WorktreeID != e.WorktreeID {
			t.Fatalf("request=%+v", request)
		}
		process.wait = func() error {
			_, outErr := io.WriteString(stdout, "setup stdout\n")
			_, errErr := io.WriteString(stderr, "Authorization: Bearer dummy-secret\nsetup stderr\n")
			return errors.Join(outErr, errErr, &sandbox.ExitError{Code: 23})
		}
		return baselineRuntimeFunc(func(_ context.Context, spec sandbox.SandboxSpec, argv []string) (sandbox.Process, error) {
			calls++
			if spec.Kind != sandbox.TestSetup || spec.Worktree != e.WorktreePath || spec.Network != sandbox.NetworkSetup || spec.ConfigDir != "" || len(spec.Env) != 0 ||
				!reflect.DeepEqual(spec.AllowedDestinations, []string{"registry.example:443"}) || !reflect.DeepEqual(argv, []string{"/bin/sh", "-c", command}) {
				t.Fatalf("unsafe runtime call: %+v %v", spec, argv)
			}
			return process, nil
		}), sandbox.SandboxSpec{Kind: sandbox.TestSetup, Worktree: e.WorktreePath, Network: sandbox.NetworkSetup, AllowedDestinations: []string{"registry.example:443"}}, []string{"/bin/sh", "-c", request.Command}, nil
	}
	if err := stage.Run(context.Background(), e); err == nil || calls != 1 || !process.waited.Load() || reports.Load() != 2 {
		t.Fatalf("err=%v calls=%d waited=%v reports=%d", err, calls, process.waited.Load(), reports.Load())
	}
	if _, err := os.Stat(canary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup executed on host: %v", err)
	}
	run, status := readSetupRun(t, f, e)
	if status != "failed" || run.ExitCode == nil || *run.ExitCode != 23 || run.TimedOut {
		t.Fatalf("status=%s run=%+v", status, run)
	}
	assertSetupTerminal(t, f, e, "failed", "config_error")
	var path string
	var redacted bool
	if err := f.db.QueryRow(`SELECT storage_path,redacted FROM artifacts WHERE execution_id=? AND kind='setup_log'`, e.Claim.Execution.ID).Scan(&path, &redacted); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(f.dataDir, path))
	if err != nil || !redacted || !strings.Contains(string(body), "setup stdout") || !strings.Contains(string(body), "setup stderr") || strings.Contains(string(body), "dummy-secret") {
		t.Fatalf("log=%q redacted=%v err=%v", body, redacted, err)
	}
	var events int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM execution_events WHERE execution_id=? AND type='log' AND json_extract(payload,'$.stream')='setup'`, e.Claim.Execution.ID).Scan(&events); err != nil || events != 3 {
		t.Fatalf("log events=%d err=%v", events, err)
	}
	// The same measurement can be resent after a lost ACK, even after failure.
	repository := remoteSetupRepository{execution: e}
	if err := repository.SaveSetup(context.Background(), e.WorktreeID, run, false); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM test_runs WHERE kind='setup'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate setup count=%d err=%v", count, err)
	}
	run.ExitCode = new(0)
	if err := repository.SaveSetup(context.Background(), e.WorktreeID, run, true); err == nil {
		t.Fatal("immutable setup measurement replaced")
	}
}

func TestSetupSandboxFailureNeverFallsBackToHost(t *testing.T) {
	for _, mode := range []string{"launch", "invalid-kind", "invalid-path", "harness-network", "harness-config", "missing-exit", "runtime-after-zero-exit"} {
		t.Run(mode, func(t *testing.T) {
			f, e, p := newSetupExecution(t)
			canary := filepath.Join(t.TempDir(), "host-executed")
			setupCommand(t, f, e, fmt.Sprintf("printf host > %q", canary))
			stage := setupStage(p)
			calls := 0
			stage.Prepare = func(_ context.Context, request SetupRequest, _, _ io.Writer) (sandbox.Sandbox, sandbox.SandboxSpec, []string, error) {
				spec := sandbox.SandboxSpec{Kind: sandbox.TestSetup, Worktree: request.WorktreePath}
				switch mode {
				case "invalid-kind":
					spec.Kind = sandbox.Harness
				case "invalid-path":
					spec.Worktree += "-other"
				case "harness-network":
					spec.Network = sandbox.NetworkHarness
				case "harness-config":
					spec.ConfigDir = "/secret"
				}
				return baselineRuntimeFunc(func(context.Context, sandbox.SandboxSpec, []string) (sandbox.Process, error) {
					calls++
					if mode == "launch" {
						return nil, errors.New("credential=do-not-leak")
					}
					process := &setupProcess{wait: func() error { return nil }}
					if mode == "runtime-after-zero-exit" {
						process.code = new(0)
						process.wait = func() error { return &sandbox.Error{Op: "cleanup"} }
					}
					return process, nil
				}), spec, []string{"/bin/sh", "-c", request.Command}, nil
			}
			if err := stage.Run(context.Background(), e); err == nil || strings.Contains(err.Error(), "do-not-leak") {
				t.Fatalf("err=%v", err)
			}
			wantCalls := 0
			if mode == "launch" || mode == "missing-exit" || mode == "runtime-after-zero-exit" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("runtime calls=%d want=%d", calls, wantCalls)
			}
			if _, err := os.Stat(canary); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("host fallback: %v", err)
			}
			_, status := readSetupRun(t, f, e)
			if status != "failed" {
				t.Fatalf("setup status=%s", status)
			}
			assertSetupTerminal(t, f, e, "failed", "sandbox_error")
		})
	}
}

func TestSetupTimeoutAndCancellationSaveRunAfterSandboxStops(t *testing.T) {
	for _, timeout := range []bool{true, false} {
		t.Run(fmt.Sprint("timeout=", timeout), func(t *testing.T) {
			f, e, p := newSetupExecution(t)
			setupCommand(t, f, e, "fixture setup")
			e.control.Timeouts.Setup = 30 * time.Millisecond
			e.control.CancelGrace = time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stage := setupStage(p)
			stopped := make(chan struct{})
			var kills atomic.Int32
			stage.Prepare = func(_ context.Context, r SetupRequest, stdout, _ io.Writer) (sandbox.Sandbox, sandbox.SandboxSpec, []string, error) {
				return baselineRuntimeFunc(func(context.Context, sandbox.SandboxSpec, []string) (sandbox.Process, error) {
					process := &setupProcess{wait: func() error { _, _ = io.WriteString(stdout, "partial setup output\n"); <-stopped; return nil }, kill: func() error { kills.Add(1); close(stopped); return nil }}
					if !timeout {
						cancel()
					}
					return process, nil
				}), sandbox.SandboxSpec{Kind: sandbox.TestSetup, Worktree: r.WorktreePath}, []string{"fixture"}, nil
			}
			err := stage.Run(ctx, e)
			wantErr, status, class := context.Canceled, "cancelled", "cancelled"
			if timeout {
				wantErr, status, class = context.DeadlineExceeded, "timed_out", "timeout"
			}
			if !errors.Is(err, wantErr) || kills.Load() != 1 {
				t.Fatalf("err=%v kills=%d", err, kills.Load())
			}
			run, setup := readSetupRun(t, f, e)
			if setup != "failed" || run.TimedOut != timeout || run.ExitCode != nil {
				t.Fatalf("setup=%s run=%+v", setup, run)
			}
			assertSetupTerminal(t, f, e, status, class)
			var artifacts int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE kind='setup_log'`).Scan(&artifacts); err != nil || artifacts != 1 {
				t.Fatalf("artifacts=%d err=%v", artifacts, err)
			}
		})
	}
}

func TestSetupSuccessReuseAndRetryPolicy(t *testing.T) {
	for _, mode := range []string{"new", "reuse-succeeded", "reuse-pending", "reuse-failed", "fresh-succeeded", "unset", "empty"} {
		t.Run(mode, func(t *testing.T) {
			f, e, p := newSetupExecution(t)
			setupCommand(t, f, e, "fixture setup")
			previous := "pending"
			if strings.HasPrefix(mode, "reuse-") {
				previous = strings.TrimPrefix(mode, "reuse-")
				e.Claim.Worktree = &ClaimWorktree{ID: e.WorktreeID, Path: e.WorktreePath, BaseSHA: e.BaseSHA, SetupStatus: previous}
			}
			if mode == "fresh-succeeded" {
				previous = "succeeded"
				e.Claim.Execution.Mode = "fresh"
			}
			if mode == "unset" {
				e.Claim.Project.SetupCommand = nil
			}
			if mode == "empty" {
				e.Claim.Project.SetupCommand = new("  ")
			}
			if _, err := f.db.Exec(`UPDATE worktrees SET setup_status=?`, previous); err != nil {
				t.Fatal(err)
			}
			stage := setupStage(p)
			calls := 0
			stage.Prepare = func(_ context.Context, r SetupRequest, _, _ io.Writer) (sandbox.Sandbox, sandbox.SandboxSpec, []string, error) {
				return baselineRuntimeFunc(func(context.Context, sandbox.SandboxSpec, []string) (sandbox.Process, error) {
					calls++
					return &setupProcess{code: new(0), wait: func() error { return nil }}, nil
				}), sandbox.SandboxSpec{Kind: sandbox.TestSetup, Worktree: r.WorktreePath}, []string{"fixture"}, nil
			}
			if err := stage.Run(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			want := 1
			if mode == "reuse-succeeded" || mode == "unset" || mode == "empty" {
				want = 0
			}
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM test_runs WHERE kind='setup'`).Scan(&count); err != nil || calls != want || count != want {
				t.Fatalf("calls=%d runs=%d want=%d err=%v", calls, count, want, err)
			}
			if want == 1 {
				run, status := readSetupRun(t, f, e)
				if status != "succeeded" || run.ExitCode == nil || *run.ExitCode != 0 {
					t.Fatalf("status=%s run=%+v", status, run)
				}
			}
		})
	}
}

func TestSetupHTTPScopeImmutabilityAndAtomicity(t *testing.T) {
	f, e, _ := newSetupExecution(t)
	setupCommand(t, f, e, "fixture setup")
	repository := remoteSetupRepository{execution: e}
	now := time.Now().UTC()
	run := model.TestRun{ID: ulid.Make().String(), ExecutionID: e.Claim.Execution.ID, Kind: "setup", TargetSHA: e.BaseSHA, Command: *e.Claim.Project.SetupCommand, ExitCode: new(0), StartedAt: now, FinishedAt: now}
	for _, change := range []func(*model.TestRun){
		func(r *model.TestRun) { r.TargetSHA = strings.Repeat("f", 40) },
		func(r *model.TestRun) { r.Command = "unregistered" },
		func(r *model.TestRun) { r.ExecutionID = ulid.Make().String() },
		func(r *model.TestRun) { r.ExitCode = nil },
	} {
		bad := run
		change(&bad)
		if err := repository.SaveSetup(context.Background(), e.WorktreeID, bad, true); err == nil {
			t.Fatalf("invalid record accepted: %+v", bad)
		}
	}
	if _, err := repository.SetupSucceeded(context.Background(), e.Claim.Execution.ID, "unrelated-worktree"); err == nil {
		t.Fatal("unrelated worktree status accessible")
	}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_setup BEFORE UPDATE OF setup_status ON worktrees BEGIN SELECT RAISE(ABORT, 'reject setup'); END`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := repository.SaveSetup(ctx, e.WorktreeID, run, true); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	var count int
	var status string
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM test_runs WHERE kind='setup'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial measurement count=%d err=%v", count, err)
	}
	if err := f.db.QueryRow(`SELECT setup_status FROM worktrees WHERE id=?`, e.WorktreeID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("partial setup status=%s err=%v", status, err)
	}
}
