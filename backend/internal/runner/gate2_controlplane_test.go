package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

const gate2BaselineXML = `<testsuite><testcase id="a"/><testcase id="b"/><testcase id="disabled"><skipped/></testcase></testsuite>`

// These rule strings and the ID resolver are fixture policies, not production
// defaults for specification gaps.
func gate2FixtureRules() Gate2Rules {
	return Gate2Rules{"fixture_missing", "fixture_unverifiable", "fixture_failure", "fixture_counts"}
}

func TestGate2HTTPContinueReusesOriginalBaselineAfterRestartAndCleanup(t *testing.T) {
	for _, cleaned := range []bool{false, true} {
		name := "Retained"
		if cleaned {
			name = "Cleaned"
		}
		t.Run(name, func(t *testing.T) {
			f := newGate2Fixture(t)
			repo := f.path
			if cleaned {
				f.path = filepath.Join(t.TempDir(), "restored")
				gate2Git(t, repo, "worktree", "add", "-b", "task", f.path, f.sha)
				gate2Write(t, filepath.Join(f.path, "cases.xml"), gate2BaselineXML)
				if _, err := f.db.Exec(`UPDATE worktrees SET path=?`, f.path); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			p, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			first, err := p.claim(ctx)
			if err != nil || first == nil {
				t.Fatalf("claim=%+v %v", first, err)
			}
			calls := 0
			if err := f.gate2Executor(t, p, &calls, nil, nil, true, nil).Execute(ctx, *first); err != nil {
				t.Fatal(err)
			}
			if cleaned {
				gate2Git(t, f.path, "add", ".")
				gate2Git(t, f.path, "commit", "--quiet", "-m", "fixture retained work")
				head := gate2Git(t, f.path, "rev-parse", "HEAD")
				gate2Git(t, repo, "worktree", "remove", f.path)
				if _, err := f.db.Exec(`UPDATE worktrees SET status='cleaned',cleaned_at='now'; UPDATE executions SET branch_name='task',harness_head_sha=? WHERE id=?`, head, first.Execution.ID); err != nil {
					t.Fatal(err)
				}
			}
			id := ulid.Make().String()
			if _, err := f.db.Exec(`INSERT INTO executions (id,task_id,seq,mode,status,created_at) VALUES (?,'task-001',2,'continue','pending','now'); UPDATE tasks SET status='queued'`, id); err != nil {
				t.Fatal(err)
			}
			p, err = New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := p.claim(ctx)
			if err != nil || claim == nil || claim.Worktree == nil || claim.Worktree.Baseline == nil || claim.Worktree.Baseline.ExecutionID != first.Execution.ID {
				t.Fatalf("continue claim=%+v %v", claim, err)
			}
			prepare := StageFunc(func(context.Context, *Execution) error { return nil })
			if cleaned {
				prepare = func(context.Context, *Execution) error {
					gate2Git(t, repo, "worktree", "add", f.path, "task")
					_, err := f.db.Exec(`UPDATE worktrees SET status='active',cleaned_at=NULL`)
					return err
				}
			}
			err = f.gate2Executor(t, p, &calls, func(context.Context, *Execution) error {
				gate2Write(t, filepath.Join(f.path, "cases.xml"), `<testsuite><testcase id="a"/><testcase id="new"/><testcase><skipped/></testcase></testsuite>`)
				return nil
			}, prepare, true, nil).Execute(ctx, *claim)
			if !errors.Is(err, ErrGate2Stopped) || calls != 3 {
				t.Fatalf("continue result=%v runtime calls=%d", err, calls)
			}
			var cause string
			var baselines, verifies int
			if err := f.db.QueryRow(`SELECT cause FROM execution_blocks WHERE execution_id=?`, id).Scan(&cause); err != nil || cause != "gate2_missing_tests" {
				t.Fatalf("cause=%s %v", cause, err)
			}
			if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM test_runs WHERE kind='baseline'),(SELECT count(*) FROM test_runs WHERE kind='verify')`).Scan(&baselines, &verifies); err != nil || baselines != 1 || verifies != 2 {
				t.Fatalf("baseline=%d verify=%d %v", baselines, verifies, err)
			}
		})
	}
}

func TestGate2ResumePointsDoNotRerunTests(t *testing.T) {
	for _, resume := range []string{"gate3", "publish"} {
		t.Run(resume, func(t *testing.T) {
			f := newGate2Fixture(t)
			p, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			config := Gate2Config{BaselineConfig: BaselineConfig{Sandbox: baselineRuntimeFunc(func(context.Context, sandbox.SandboxSpec, []string) (sandbox.Process, error) {
				t.Fatal("resumed Gate 2 invoked sandbox")
				return nil, nil
			}), Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 1 << 20, Time: time.Minute}, Shell: "/bin/sh", JUnitFormat: "junit"}, Rules: gate2FixtureRules()}
			handler, err := p.Gate2Handler(config)
			if err != nil {
				t.Fatal(err)
			}
			stopped := errors.New("fixture reached resume point")
			handlers := stubStageHandlers(func(id StageID, _ context.Context, _ *Execution) error {
				if string(id) != resume {
					t.Fatalf("ran stage %s before resume %s", id, resume)
				}
				return stopped
			})
			handlers[StageGate2] = handler
			executor, err := NewExecutor(handlers)
			if err != nil {
				t.Fatal(err)
			}
			var claim Claim
			claim.Execution.ResumePoint = &resume
			if err := executor.Execute(context.Background(), claim); !errors.Is(err, stopped) {
				t.Fatalf("resume result=%v", err)
			}
		})
	}
}

func gate2Git(t *testing.T, path string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", path, "-c", "core.hooksPath=/dev/null", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
	body, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, body, err)
	}
	return strings.TrimSpace(string(body))
}

func gate2Write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *baselineHTTPFixture) gate2Executor(t *testing.T, runner *Runner, calls *int, harness, prepare StageFunc, resolver bool, runtimeErr error) *Executor {
	t.Helper()
	config := BaselineConfig{Sandbox: baselineRuntimeFunc(func(_ context.Context, spec sandbox.SandboxSpec, argv []string) (sandbox.Process, error) {
		*calls++
		if spec.Kind != sandbox.TestRun || spec.Network != sandbox.NetworkNone || spec.GitDir != "" || len(spec.Env) != 0 || len(argv) != 3 || argv[2] != f.command {
			t.Fatalf("sandbox request=%+v %v", spec, argv)
		}
		if runtimeErr != nil && *calls > 1 {
			return nil, runtimeErr
		}
		command := exec.Command(argv[0], argv[1:]...)
		command.Dir, command.Env = spec.Worktree, []string{"PATH=/usr/bin:/bin"}
		if err := command.Start(); err != nil {
			return nil, err
		}
		return &baselineCommandProcess{command}, nil
	}), Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 1 << 20, Time: time.Minute}, Shell: "/bin/sh", JUnitFormat: "junit"}
	if resolver {
		config.Resolver = reportIDResolver{}
	}
	gateConfig := Gate2Config{BaselineConfig: config, Rules: gate2FixtureRules(), LoadTestIDs: func(ctx context.Context, run model.TestRun) ([]string, error) {
		// Trusted artifact read is explicitly injected because the specs expose
		// artifact GET only to the owner, not to the Runner.
		var path string
		if err := f.db.QueryRowContext(ctx, `SELECT storage_path FROM artifacts WHERE id=? AND execution_id=? AND kind='test_ids'`, *run.TestIDsArtifactID, run.ExecutionID).Scan(&path); err != nil {
			return nil, err
		}
		body, err := os.ReadFile(filepath.Join(f.dataDir, path))
		if err != nil {
			return nil, err
		}
		var ids []string
		err = json.Unmarshal(body, &ids)
		return ids, err
	}}
	handlers := stubStageHandlers(func(id StageID, ctx context.Context, e *Execution) error {
		switch id {
		case StagePrepareWorktree:
			if prepare != nil {
				if err := prepare(ctx, e); err != nil {
					return err
				}
			}
			e.WorktreeID, e.WorktreePath, e.BaseSHA = f.worktreeID, f.path, f.sha
		case StageHarness:
			if harness != nil {
				return harness(ctx, e)
			}
		case StageGate3:
			if e.VerifyRun == nil || e.VerifiedSHA != e.HarnessHeadSHA || e.VerifyRun.TargetSHA != e.VerifiedSHA {
				t.Fatalf("Gate 3 missing verification: %+v", e)
			}
		case StagePublish:
			var n int
			if err := f.db.QueryRow(`SELECT count(*) FROM test_runs WHERE kind='verify' AND execution_id=?`, e.Claim.Execution.ID).Scan(&n); err != nil || n != 1 {
				t.Fatalf("publication before verify persistence: %d %v", n, err)
			}
		}
		return nil
	})
	control := testExecutionControl()
	control.Reporter, control.CheckCancellation = runner, func(context.Context) error { return nil }
	executor, err := runner.NewGate2Executor(handlers, control, gateConfig)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func newGate2Fixture(t *testing.T) *baselineHTTPFixture {
	t.Helper()
	f := newBaselineHTTPFixture(t)
	f.command = `if test -f cases.xml; then cat cases.xml > report.xml; fi; printf x >> calls; test ! -f fail`
	if _, err := f.db.Exec(`UPDATE projects SET test_command=?`, f.command); err != nil {
		t.Fatal(err)
	}
	gate2Write(t, filepath.Join(f.path, "cases.xml"), gate2BaselineXML)
	return f
}

func TestGate2HTTPAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name, report, status, cause, class         string
		counts, noCommand, nonzero, sandboxFailure bool
		runtimeErr                                 error
	}{
		{name: "UnsetCommand", noCommand: true, status: "failed", class: "config_error"},
		{name: "MissingExistingID", report: `<testsuite><testcase id="a"/><testcase id="new"/><testcase id="disabled"><skipped/></testcase></testsuite>`, status: "blocked", cause: "gate2_missing_tests"},
		{name: "IncreasedSkip", report: `<testsuite><testcase id="a"/><testcase id="b"/><testcase id="n"/><testcase><skipped/></testcase><testcase><skipped/></testcase></testsuite>`, status: "blocked", cause: "gate2_missing_tests"},
		{name: "AddedTests", report: `<testsuite><testcase id="a"/><testcase id="b"/><testcase id="new"/><testcase><skipped/></testcase></testsuite>`, status: "succeeded"},
		{name: "NoReport", status: "blocked", cause: "gate2_unverifiable"},
		{name: "MalformedReport", report: `<testsuite>`, status: "blocked", cause: "gate2_unverifiable"},
		{name: "CountOnlyAddedTests", counts: true, report: `<testsuite tests="5" skipped="1"/>`, status: "succeeded"},
		{name: "CountOnlyFewerExecuted", counts: true, report: `<testsuite tests="2" skipped="1"/>`, status: "blocked", cause: "gate2_missing_tests"},
		{name: "CountOnlyMoreSkipped", counts: true, report: `<testsuite tests="5" skipped="2"/>`, status: "blocked", cause: "gate2_missing_tests"},
		{name: "TestFailure", report: `<testsuite><testcase id="a"><failure/></testcase><testcase id="b"/></testsuite>`, status: "failed", class: "test_failed"},
		{name: "NonzeroWithoutReport", nonzero: true, status: "failed", class: "test_failed"},
		{name: "SandboxCreationFailure", sandboxFailure: true, status: "failed", class: "sandbox_error"},
		{name: "Timeout", runtimeErr: context.DeadlineExceeded, status: "timed_out", class: "timeout"},
		{name: "Cancellation", runtimeErr: context.Canceled, status: "cancelled", class: "cancelled"},
		{name: "IDReportBecomesCounts", report: `<testsuite tests="4" skipped="1"/>`, status: "succeeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGate2Fixture(t)
			if tc.noCommand {
				if _, err := f.db.Exec(`UPDATE projects SET test_command=NULL`); err != nil {
					t.Fatal(err)
				}
			}
			p, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			claim, err := p.claim(ctx)
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v %v", claim, err)
			}
			calls := 0
			runtimeErr := tc.runtimeErr
			if tc.sandboxFailure {
				runtimeErr = errors.New("fixture sandbox unavailable")
			}
			executor := f.gate2Executor(t, p, &calls, func(context.Context, *Execution) error {
				if tc.report == "" {
					if err := os.Remove(filepath.Join(f.path, "cases.xml")); err != nil {
						t.Fatal(err)
					}
				} else {
					gate2Write(t, filepath.Join(f.path, "cases.xml"), tc.report)
				}
				if tc.nonzero {
					gate2Write(t, filepath.Join(f.path, "fail"), "")
				}
				gate2Git(t, f.path, "commit", "--quiet", "--allow-empty", "-m", "harness")
				return nil
			}, nil, !tc.counts, runtimeErr)
			err = executor.Execute(ctx, *claim)
			if tc.runtimeErr != nil && !errors.Is(err, tc.runtimeErr) || tc.status == "succeeded" && err != nil || tc.status != "succeeded" && tc.runtimeErr == nil && !errors.Is(err, ErrGate2Stopped) {
				t.Fatalf("execution error=%v", err)
			}
			var status, class, verified, head, cause string
			if err := f.db.QueryRow(`SELECT status,coalesce(error_class,''),coalesce(verified_sha,''),coalesce(harness_head_sha,'') FROM executions WHERE id=?`, claim.Execution.ID).Scan(&status, &class, &verified, &head); err != nil {
				t.Fatal(err)
			}
			if status != tc.status || class != tc.class || head == f.sha || (status == "succeeded") != (verified == head) {
				t.Fatalf("status=%s class=%s verified=%s head=%s", status, class, verified, head)
			}
			if tc.cause != "" {
				if err := f.db.QueryRow(`SELECT cause FROM execution_blocks WHERE execution_id=? AND gate_result_id IN (SELECT id FROM gate_results WHERE gate=2)`, claim.Execution.ID).Scan(&cause); err != nil || cause != tc.cause {
					t.Fatalf("block=%s %v", cause, err)
				}
			}
			wantCalls := 2
			if tc.noCommand {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("sandbox calls=%d want=%d", calls, wantCalls)
			}
			var runs, gates, events int
			for _, query := range []struct {
				sql   string
				value *int
			}{
				{`SELECT count(*) FROM test_runs WHERE kind='verify' AND execution_id=?`, &runs},
				{`SELECT count(*) FROM gate_results WHERE gate=2 AND execution_id=?`, &gates},
				{`SELECT count(*) FROM execution_events WHERE type='gate_result' AND execution_id=?`, &events},
			} {
				if err := f.db.QueryRow(query.sql, claim.Execution.ID).Scan(query.value); err != nil {
					t.Fatal(err)
				}
			}
			wantRuns := 1
			if tc.noCommand {
				wantRuns = 0
			}
			wantGates := 1
			if tc.runtimeErr != nil {
				wantGates = 0
			}
			if runs != wantRuns || gates != wantGates || events != wantGates {
				t.Fatalf("runs=%d gates=%d events=%d", runs, gates, events)
			}
			if (tc.counts || tc.name == "IDReportBecomesCounts") && status == "succeeded" {
				var findings string
				if err := f.db.QueryRow(`SELECT findings FROM gate_results WHERE execution_id=?`, claim.Execution.ID).Scan(&findings); err != nil || !strings.Contains(findings, "件数による確認") {
					t.Fatalf("count confirmation=%s %v", findings, err)
				}
			}
			if tc.name == "AddedTests" {
				var total int
				var artifact, kind, path string
				if err := f.db.QueryRow(`SELECT r.total,a.id,a.kind,a.storage_path FROM test_runs r JOIN artifacts a ON a.id=r.test_ids_artifact_id WHERE r.kind='verify' AND r.execution_id=?`, claim.Execution.ID).Scan(&total, &artifact, &kind, &path); err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(filepath.Join(f.dataDir, path))
				if err != nil || string(body) != `["a","b","new"]` || total != 4 || kind != "test_ids" {
					t.Fatalf("verify artifact=%s total=%d kind=%s %v", body, total, kind, err)
				}
			}
		})
	}
}
