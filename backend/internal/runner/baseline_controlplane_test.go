package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Only the isolated runtime is replaced. It executes the actual argv supplied
// by product Prepare, producing a file subsequently read by product OpenReport.
// This is a test double, never a production host-execution fallback.
type baselineCommandProcess struct{ command *exec.Cmd }

func (p *baselineCommandProcess) Wait() error {
	err := p.command.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return &sandbox.ExitError{Code: exit.ExitCode()}
	}
	return err
}
func (p *baselineCommandProcess) Stop() error { return p.command.Process.Kill() }
func (p *baselineCommandProcess) Kill() error { return p.command.Process.Kill() }
func (p *baselineCommandProcess) ExitCode() (int, bool) {
	if p.command.ProcessState == nil {
		return 0, false
	}
	return p.command.ProcessState.ExitCode(), true
}

type baselineHTTPFixture struct {
	*logControlPlane
	path, sha, worktreeID string
	command               string
}

func newBaselineHTTPFixture(t *testing.T) *baselineHTTPFixture {
	t.Helper()
	f := &baselineHTTPFixture{logControlPlane: newLogControlPlane(t), path: t.TempDir(), worktreeID: ulid.Make().String()}
	for _, args := range [][]string{{"init", "--quiet"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "--allow-empty", "-m", "start"}} {
		cmd := exec.Command("git", append([]string{"-C", f.path}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	cmd := exec.Command("git", "-C", f.path, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	f.sha = strings.TrimSpace(string(out))
	f.command = `test 2 -gt 1 && printf '%s' '<testsuite><testcase name="existing-a"/><testcase name="existing-b"/><testcase name="disabled"><skipped/></testcase></testsuite>' > report.xml && printf x >> calls`
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`UPDATE projects SET test_command = ?, test_report_format = 'junit', test_report_path = 'report.xml'`, []any{f.command}},
		{`UPDATE tasks SET status = 'queued'`, nil},
		{`UPDATE executions SET status = 'pending', runner_id = NULL`, nil},
		{`INSERT INTO harnesses (id,name,specialty,supported_tasks,adapter,repo_path,repo_url,supported_models,available_tools,tool_permissions,contract_version,created_at,updated_at)
		VALUES ('h','Harness','developer','["development"]','codex','/h','','[]','[]','{}','1','now','now')`, nil},
		{`INSERT INTO harness_profiles (id,harness_id,version,commit_sha,tag_name,definition,definition_hash,model_id,operation_status,registered_at)
		VALUES ('p','h','1.0.0',?,'v1','{}',?,'test','provisional','now')`, []any{strings.Repeat("a", 40), strings.Repeat("b", 64)}},
		{`INSERT INTO project_harnesses VALUES ('project','developer','h','p','now')`, nil},
		{`INSERT INTO worktrees (id,project_id,task_id,runner_id,path,branch_name,base_sha,status,setup_status,copied_files,created_at)
		VALUES (?,'project','task-001','mac-mini',?,'task',?,'active','succeeded','[]','now')`, []any{f.worktreeID, f.path, f.sha}},
	} {
		if _, err := f.db.Exec(stmt.query, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *baselineHTTPFixture) executor(t *testing.T, process *Runner, calls *int) *Executor {
	return f.executorWithPrepare(t, process, calls, nil)
}

// Worktree creation/restoration belongs to the preparation stage, outside the
// baseline handler. Tests can supply that boundary while retaining all later
// product stages, HTTP reporting and baseline persistence.
func (f *baselineHTTPFixture) executorWithPrepare(t *testing.T, process *Runner, calls *int, prepare StageFunc) *Executor {
	t.Helper()
	handler, err := process.BaselineHandler(BaselineConfig{
		Sandbox: baselineRuntimeFunc(func(_ context.Context, spec sandbox.SandboxSpec, argv []string) (sandbox.Process, error) {
			*calls++
			if spec.Kind != sandbox.TestRun || spec.Network != sandbox.NetworkNone || spec.GitDir != "" || spec.ConfigDir != "" || len(spec.Env) != 0 ||
				spec.Worktree != f.path || !reflect.DeepEqual(argv, []string{"/bin/sh", "-c", f.command}) {
				t.Fatalf("unexpected test runtime request: %+v %v", spec, argv)
			}
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Dir, cmd.Env = spec.Worktree, []string{"PATH=/usr/bin:/bin"}
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return &baselineCommandProcess{cmd}, nil
		}),
		Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 1 << 20, Time: time.Minute},
		Shell:  "/bin/sh", JUnitFormat: "junit",
	})
	if err != nil {
		t.Fatal(err)
	}
	handlers := stubStageHandlers(func(id StageID, ctx context.Context, e *Execution) error {
		if id == StagePrepareWorktree {
			if prepare != nil {
				if err := prepare(ctx, e); err != nil {
					return err
				}
			}
			e.WorktreeID, e.WorktreePath, e.BaseSHA = f.worktreeID, f.path, f.sha
		}
		if id == StageHarness {
			// The actual harness boundary must see a durable record, never an
			// in-memory measurement awaiting a later transition report.
			var rows int
			if err := f.db.QueryRow(`SELECT count(*) FROM test_runs WHERE kind='baseline'`).Scan(&rows); err != nil || rows != 1 || e.Baseline == nil {
				t.Fatalf("harness before persistence: %d %v", rows, err)
			}
		}
		return nil
	})
	handlers[StageTestBaseline] = handler
	control := testExecutionControl()
	control.Reporter, control.CheckCancellation = process, func(context.Context) error { return nil }
	executor, err := NewExecutorWithControl(handlers, control)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func TestBaselineHTTPContinueAfterFailureBeforeWorktreeAcquiresBaseline(t *testing.T) {
	for _, omitSnapshot := range []bool{false, true} {
		name := "explicit null allows first acquisition"
		if omitSnapshot {
			name = "omitted snapshot stops acquisition"
		}
		t.Run(name, func(t *testing.T) {
			f := newBaselineHTTPFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			process, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			first, err := process.claim(ctx)
			if err != nil || first == nil {
				t.Fatalf("first claim: %v", err)
			}
			calls := 0
			prepareErr := errors.New("worktree creation failed")
			err = f.executorWithPrepare(t, process, &calls, func(context.Context, *Execution) error {
				return prepareErr
			}).Execute(ctx, *first)
			if !errors.Is(err, prepareErr) || calls != 0 {
				t.Fatalf("first preparation: %v calls=%d", err, calls)
			}
			if err := process.ReportResult(ctx, first.Execution.ID, ExecutionResult{
				FromStatus: model.ExecutionPreparing, Status: model.ExecutionFailed, ErrorClass: model.ErrorConfig,
			}); err != nil {
				t.Fatal(err)
			}
			var worktreeID *string
			if err := f.db.QueryRow(`SELECT worktree_id FROM executions WHERE id=?`, first.Execution.ID).Scan(&worktreeID); err != nil || worktreeID != nil {
				t.Fatalf("failed preparation recorded a worktree: %v %v", worktreeID, err)
			}
			id := ulid.Make().String()
			if _, err := f.db.Exec(`INSERT INTO executions (id,task_id,seq,mode,status,created_at) VALUES (?,'task-001',2,'continue','pending','now');
				UPDATE tasks SET status='queued'`, id); err != nil {
				t.Fatal(err)
			}
			process, err = New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			if omitSnapshot {
				process.client.Transport = usageRetryTransport(func(r *http.Request) (*http.Response, error) {
					response, err := http.DefaultTransport.RoundTrip(r)
					if err != nil || !strings.HasSuffix(r.URL.Path, "/claim") {
						return response, err
					}
					var body map[string]json.RawMessage
					err = json.NewDecoder(response.Body).Decode(&body)
					response.Body.Close()
					if err != nil {
						return nil, err
					}
					delete(body, "worktree")
					encoded, err := json.Marshal(body)
					response.Body = io.NopCloser(bytes.NewReader(encoded))
					return response, err
				})
			}
			claim, err := process.claim(ctx)
			if err != nil || claim == nil || claim.Worktree != nil || claim.Execution.ID != id || claim.worktreeSnapshotPresent == omitSnapshot {
				t.Fatalf("continue snapshot: %+v %v", claim, err)
			}
			err = f.executor(t, process, &calls).Execute(ctx, *claim)
			if omitSnapshot {
				var stageErr *StageError
				if !errors.As(err, &stageErr) || stageErr.Stage.ID != StageTestBaseline || !strings.Contains(stageErr.Err.Error(), "lacks saved worktree") || calls != 0 {
					t.Fatalf("missing snapshot caused acquisition: %v calls=%d", err, calls)
				}
				var rows int
				if err := f.db.QueryRow(`SELECT count(*) FROM test_runs WHERE kind='baseline'`).Scan(&rows); err != nil || rows != 0 {
					t.Fatalf("missing snapshot saved baseline: rows=%d err=%v", rows, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			run, err := (store.BaselineStore{DB: f.db}).FindBaseline(ctx, id, f.worktreeID)
			if err != nil || run == nil || run.ExecutionID != id || run.Kind != "baseline" || run.TargetSHA != f.sha || run.Total == nil || *run.Total != 3 || calls != 1 {
				t.Fatalf("continue first baseline: %+v calls=%d err=%v", run, calls, err)
			}
		})
	}
}

func TestBaselineHTTPPersistsStartSHAReportAndIDArtifact(t *testing.T) {
	f := newBaselineHTTPFixture(t)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claim, err := process.claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	calls := 0
	if err := f.executor(t, process, &calls).Execute(ctx, *claim); err != nil {
		t.Fatal(err)
	}
	run, err := (store.BaselineStore{DB: f.db}).FindBaseline(ctx, claim.Execution.ID, f.worktreeID)
	if err != nil || run == nil {
		t.Fatalf("saved baseline: %+v %v", run, err)
	}
	if run.Kind != "baseline" || run.TargetSHA != f.sha || run.Command != f.command || run.ExitCode == nil || *run.ExitCode != 0 ||
		run.Total == nil || *run.Total != 3 || run.Passed == nil || *run.Passed != 2 || run.Failed == nil || *run.Failed != 0 || run.Skipped == nil || *run.Skipped != 1 || calls != 1 || run.TestIDsArtifactID == nil {
		t.Fatalf("unexpected baseline: %+v runtime calls=%d", run, calls)
	}
	var kind, owner, path string
	if err := f.db.QueryRow(`SELECT kind, execution_id, storage_path FROM artifacts WHERE id=?`, *run.TestIDsArtifactID).Scan(&kind, &owner, &path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(f.dataDir, path))
	var ids []string
	if err != nil || json.Unmarshal(body, &ids) != nil || !reflect.DeepEqual(ids, []string{"existing-a", "existing-b"}) || kind != "test_ids" || owner != claim.Execution.ID {
		t.Fatalf("test_ids artifact: %s %s %q %v", kind, owner, body, err)
	}
}

func TestBaselineHTTPContinueAfterRunnerRestartDoesNotReacquire(t *testing.T) {
	f := newBaselineHTTPFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	first, err := process.claim(ctx)
	if err != nil || first == nil {
		t.Fatalf("first claim: %v", err)
	}
	calls := 0
	if err := f.executor(t, process, &calls).Execute(ctx, *first); err != nil {
		t.Fatal(err)
	}
	original, err := (store.BaselineStore{DB: f.db}).FindBaseline(ctx, first.Execution.ID, f.worktreeID)
	if err != nil {
		t.Fatal(err)
	}
	// Real continue input arrives through claim after a different Execution and
	// Runner instance. Registry changes and a changed HEAD must not cause retest.
	cmd := exec.Command("git", "-C", f.path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "--allow-empty", "-m", "harness-change")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %s %v", out, err)
	}
	for seq := 2; seq <= 3; seq++ {
		id := ulid.Make().String()
		if _, err := f.db.Exec(`INSERT INTO executions (id,task_id,seq,mode,status,created_at) VALUES (?,'task-001',?,'continue','pending','now')`, id, seq); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`UPDATE tasks SET status='queued'; UPDATE projects SET test_command='must never run'`); err != nil {
			t.Fatal(err)
		}
		process, err = New(f.config)
		if err != nil {
			t.Fatal(err)
		}
		claim, err := process.claim(ctx)
		if err != nil || claim == nil || claim.Worktree == nil || claim.Worktree.Baseline == nil || !reflect.DeepEqual(&claim.Worktree.Baseline.TestRun, original) {
			t.Fatalf("continue snapshot: %+v %v", claim, err)
		}
		if err := f.executor(t, process, &calls).Execute(ctx, *claim); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := f.db.QueryRow(`SELECT count(*) FROM test_runs WHERE kind='baseline'`).Scan(&rows); err != nil || rows != 1 || calls != 1 {
		t.Fatalf("continue reacquired: rows=%d calls=%d err=%v", rows, calls, err)
	}
	got, err := os.ReadFile(filepath.Join(f.path, "calls"))
	if err != nil || string(got) != "x" {
		t.Fatalf("test command repeated: %q %v", got, err)
	}
}

func TestBaselineHTTPContinueRestoresCleanedWorktreeWithoutReacquiring(t *testing.T) {
	f := newBaselineHTTPFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Use a real linked worktree so cleanup removes its directory but leaves
	// its branch and measured start commit available for restoration.
	repository := f.path
	f.path = filepath.Join(t.TempDir(), "task")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repository, "-c", "core.hooksPath=/dev/null"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("worktree", "add", "-b", "task", f.path, f.sha)
	if _, err := f.db.Exec(`UPDATE worktrees SET path=? WHERE id=?`, f.path, f.worktreeID); err != nil {
		t.Fatal(err)
	}
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	first, err := process.claim(ctx)
	if err != nil || first == nil {
		t.Fatalf("first claim: %v", err)
	}
	calls := 0
	if err := f.executor(t, process, &calls).Execute(ctx, *first); err != nil {
		t.Fatal(err)
	}
	original, err := (store.BaselineStore{DB: f.db}).FindBaseline(ctx, first.Execution.ID, f.worktreeID)
	if err != nil || original == nil {
		t.Fatalf("original baseline: %+v %v", original, err)
	}
	git("-C", f.path, "add", "--", "report.xml", "calls")
	git("-C", f.path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "-m", "harness-change")
	head := git("-C", f.path, "rev-parse", "HEAD")
	git("worktree", "remove", f.path)
	if _, err := os.Stat(f.path); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still present: %v", err)
	}
	if git("rev-parse", "task") != head || head == f.sha {
		t.Fatal("cleanup did not preserve the changed branch")
	}
	id := ulid.Make().String()
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`UPDATE worktrees SET status='cleaned', cleaned_at='now' WHERE id=?`, []any{f.worktreeID}},
		{`UPDATE executions SET branch_name='task', harness_head_sha=? WHERE id=?`, []any{head, first.Execution.ID}},
		{`INSERT INTO executions (id,task_id,seq,mode,status,created_at) VALUES (?,'task-001',2,'continue','pending','now')`, []any{id}},
		{`UPDATE tasks SET status='queued'; UPDATE projects SET test_command='must never run'`, nil},
	} {
		if _, err := f.db.Exec(stmt.query, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	process, err = New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := process.claim(ctx)
	if err != nil || claim == nil || claim.Execution.ID != id || claim.Worktree == nil {
		t.Fatalf("cleaned worktree claim: %+v %v", claim, err)
	}
	w := claim.Worktree
	if w.ID != f.worktreeID || w.Path != f.path || w.BaseSHA != f.sha || w.Baseline == nil || !reflect.DeepEqual(&w.Baseline.TestRun, original) ||
		claim.Previous == nil || claim.Previous.BranchName == nil || *claim.Previous.BranchName != "task" ||
		claim.Previous.WorktreePath == nil || *claim.Previous.WorktreePath != f.path {
		t.Fatalf("restoration metadata: worktree=%+v previous=%+v", w, claim.Previous)
	}
	var status string
	if err := f.db.QueryRow(`SELECT status FROM executions WHERE id=?`, id).Scan(&status); err != nil || status != "claimed" {
		t.Fatalf("claim left execution %s: %v", status, err)
	}
	prepared := false
	executor := f.executorWithPrepare(t, process, &calls, func(_ context.Context, e *Execution) error {
		git("worktree", "add", e.Claim.Worktree.Path, *e.Claim.Previous.BranchName)
		if git("-C", f.path, "rev-parse", "HEAD") != head {
			t.Fatal("restoration changed the branch HEAD")
		}
		_, err := f.db.Exec(`UPDATE worktrees SET status='active', cleaned_at=NULL WHERE id=?`, e.Claim.Worktree.ID)
		prepared = err == nil
		return err
	})
	if err := executor.Execute(ctx, *claim); err != nil {
		t.Fatal(err)
	}
	run, err := (store.BaselineStore{DB: f.db}).FindBaseline(ctx, id, f.worktreeID)
	if err != nil || !reflect.DeepEqual(run, original) || !prepared || calls != 1 {
		t.Fatalf("restored baseline: %+v prepared=%v calls=%d err=%v", run, prepared, calls, err)
	}
	var rows int
	if err := f.db.QueryRow(`SELECT count(*) FROM test_runs WHERE kind='baseline'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("baseline rows=%d err=%v", rows, err)
	}
	got, err := os.ReadFile(filepath.Join(f.path, "calls"))
	if err != nil || string(got) != "x" {
		t.Fatalf("restoration repeated test command: %q %v", got, err)
	}
}

func TestBaselineHTTPRetriesLostAcknowledgementsWithoutRerunning(t *testing.T) {
	f := newBaselineHTTPFixture(t)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claim, err := process.claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	process.pollInterval = time.Millisecond
	lostResult, lostArtifact := false, false
	process.client.Transport = usageRetryTransport(func(r *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			return response, err
		}
		lose := false
		if strings.HasSuffix(r.URL.Path, "/artifacts") && !lostArtifact {
			lostArtifact, lose = true, true
		}
		if strings.HasSuffix(r.URL.Path, "/result") && response.StatusCode == 204 && !lostResult {
			// Ordinary progress reports must keep their existing 409 contract.
			var exists int
			if err := f.db.QueryRow(`SELECT count(*) FROM test_runs`).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists == 1 {
				lostResult, lose = true, true
			}
		}
		if lose {
			response.Body.Close()
			return nil, errors.New("lost acknowledgement")
		}
		return response, nil
	})
	calls := 0
	if err := f.executor(t, process, &calls).Execute(ctx, *claim); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"test_runs", "artifacts"} {
		var n int
		if err := f.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s rows=%d err=%v", table, n, err)
		}
	}
	if !lostArtifact || !lostResult || calls != 1 {
		t.Fatalf("lost artifact=%v result=%v runtime calls=%d", lostArtifact, lostResult, calls)
	}
}
