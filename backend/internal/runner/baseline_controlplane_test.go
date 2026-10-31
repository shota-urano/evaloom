package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/gate"
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

type reportIDResolver struct{}

func (reportIDResolver) ResolveJUnitID(id gate.JUnitTestID) (string, bool) { return id.ID, id.ID != "" }

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
	f.command = `test 2 -gt 1 && printf '%s' '<testsuite><testcase id="existing-a"/><testcase id="existing-b"/><testcase id="disabled"><skipped/></testcase></testsuite>' > report.xml && printf x >> calls`
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
		Shell:  "/bin/sh", JUnitFormat: "junit", Resolver: reportIDResolver{},
	})
	if err != nil {
		t.Fatal(err)
	}
	handlers := stubStageHandlers(func(id StageID, _ context.Context, e *Execution) error {
		if id == StagePrepareWorktree {
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
		if err != nil || claim == nil || claim.Worktree == nil || !reflect.DeepEqual(claim.Worktree.Baseline, original) {
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
