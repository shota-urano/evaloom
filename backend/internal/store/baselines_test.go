package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/gate"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/runner"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

type baselineSandboxFunc func(context.Context, *runner.Execution, runner.BaselineTestRequest) (runner.BaselineTestOutput, error)

func (f baselineSandboxFunc) RunBaseline(ctx context.Context, e *runner.Execution, r runner.BaselineTestRequest) (runner.BaselineTestOutput, error) {
	return f(ctx, e, r)
}

func baselineFixture(t *testing.T) (*sql.DB, runner.BaselineStage, *runner.Execution, *int) {
	t.Helper()
	db := openTaskTables(t)
	sha := strings.Repeat("a", 40)
	mustTaskExec(t, db, `UPDATE executions SET worktree_id = 'worktree-1', base_sha = ?, runner_id = 'mac-mini' WHERE id = 'execution-1'`, sha)
	mustTaskExec(t, db, `UPDATE worktrees SET setup_status = 'succeeded' WHERE id = 'worktree-1'`)
	e := &runner.Execution{WorktreeID: "worktree-1", WorktreePath: "/worktrees/task-001", BaseSHA: sha}
	e.Claim.Execution.ID, e.Claim.Execution.TaskID, e.Claim.Execution.Mode = "execution-1", "task-001", "fresh"
	e.Claim.Project.TestCommand = new("make test")
	e.Claim.Project.TestReportFormat, e.Claim.Project.TestReportPath = new("fixture-junit"), new("reports/test.xml")
	artifacts, err := NewArtifactStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { artifacts.Close() })
	calls := new(int)
	stage := runner.BaselineStage{
		Repository: BaselineStore{DB: db},
		Sandbox: baselineSandboxFunc(func(_ context.Context, execution *runner.Execution, request runner.BaselineTestRequest) (runner.BaselineTestOutput, error) {
			*calls++
			if request.Kind != sandbox.TestRun || request.TargetSHA != sha || request.Command != "make test" ||
				request.WorktreeID != execution.WorktreeID || request.WorktreePath != execution.WorktreePath ||
				request.ReportFormat == nil || *request.ReportFormat != "fixture-junit" || request.ReportPath == nil || *request.ReportPath != "reports/test.xml" {
				t.Fatalf("baseline did not use the start SHA and Registry settings: %+v", request)
			}
			return runner.BaselineTestOutput{TargetSHA: sha, ExitCode: new(0), Report: io.NopCloser(strings.NewReader(
				`<testsuite><testcase name="existing-a"/><testcase name="existing-b"/><testcase name="disabled"><skipped/></testcase></testsuite>`))}, nil
		}),
		Parse: gate.ParseJUnitXML,
		// Use the production JSON test_ids representation.
		RecordIDs: func(ctx context.Context, executionID string, ids []string) (string, error) {
			body, err := json.Marshal(ids)
			if err != nil {
				return "", err
			}
			id := "ids-" + executionID
			_, err = artifacts.Save(ctx, id, executionID, "test_ids", false, strings.NewReader(string(body)))
			return id, err
		},
	}
	return db, stage, e, calls
}

func TestBaselineStageSavesStartSHAAndMeasurements(t *testing.T) {
	db, stage, e, calls := baselineFixture(t)
	// Exercise the real stage table: setup must finish before acquisition, and
	// the harness may start only after the record is durably available.
	setup, harness := false, false
	handlers := map[runner.StageID]runner.StageFunc{}
	for _, step := range runner.ExecutionStages() {
		handlers[step.ID] = func(context.Context, *runner.Execution) error { return nil }
	}
	handlers[runner.StagePrepareWorktree] = func(_ context.Context, execution *runner.Execution) error {
		execution.WorktreeID, execution.WorktreePath, execution.BaseSHA = e.WorktreeID, e.WorktreePath, e.BaseSHA
		return nil
	}
	handlers[runner.StageSetup] = func(context.Context, *runner.Execution) error { setup = true; return nil }
	handlers[runner.StageTestBaseline] = func(ctx context.Context, execution *runner.Execution) error {
		if !setup {
			t.Fatal("baseline ran before setup")
		}
		return stage.Run(ctx, execution)
	}
	handlers[runner.StageHarness] = func(ctx context.Context, execution *runner.Execution) error {
		harness = true
		got, err := stage.Repository.FindBaseline(ctx, execution.Claim.Execution.ID, execution.WorktreeID)
		if err != nil || got == nil || !reflect.DeepEqual(got, execution.Baseline) {
			t.Fatalf("harness started before baseline was durable: %+v %v", got, err)
		}
		return nil
	}
	executor, err := runner.NewExecutor(handlers)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(context.Background(), e.Claim); err != nil {
		t.Fatal(err)
	}
	var kind, sha, command, ids string
	var exit, total, passed, failed, skipped int
	if err := db.QueryRow(`SELECT kind, target_sha, command, exit_code, total, passed, failed, skipped, test_ids_artifact_id FROM test_runs`).Scan(
		&kind, &sha, &command, &exit, &total, &passed, &failed, &skipped, &ids); err != nil {
		t.Fatal(err)
	}
	if kind != "baseline" || sha != e.BaseSHA || command != "make test" || exit != 0 || total != 3 || passed != 2 || failed != 0 || skipped != 1 || *calls != 1 || !harness {
		t.Fatalf("saved baseline = %s %s %s %d %d %d %d %d, calls=%d harness=%v", kind, sha, command, exit, total, passed, failed, skipped, *calls, harness)
	}
	var artifactKind, owner, path string
	if err := db.QueryRow(`SELECT kind, execution_id, storage_path FROM artifacts WHERE id = ?`, ids).Scan(&artifactKind, &owner, &path); err != nil || artifactKind != "test_ids" || owner != "execution-1" || path == "" {
		t.Fatalf("baseline IDs not persisted as an owned artifact: %s %s %s %v", artifactKind, owner, path, err)
	}
}

func TestBaselineContinueReusesOriginalRunWithoutTestExecution(t *testing.T) {
	db, stage, e, calls := baselineFixture(t)
	ctx := context.Background()
	if err := stage.Run(ctx, e); err != nil {
		t.Fatal(err)
	}
	original := *e.Baseline
	// An unrelated verify row must not be mistaken for the start baseline.
	mustTaskExec(t, db, `INSERT INTO test_runs SELECT 'verify-1', execution_id, 'verify', ?, command, exit_code,
		total, passed, failed, skipped, test_ids_artifact_id, timed_out, started_at, finished_at FROM test_runs`, strings.Repeat("b", 40))
	if _, err := insertExecution(db, "execution-2", "task-001", 2, "pending"); err != nil {
		t.Fatal(err)
	}
	mustTaskExec(t, db, `UPDATE executions SET worktree_id = 'worktree-1', base_sha = ?, runner_id = 'mac-mini' WHERE id = 'execution-2'`, e.BaseSHA)
	e.Claim.Execution.ID, e.Claim.Execution.Mode = "execution-2", "continue"
	e.Claim.Project.TestCommand = new("different Registry command")
	// Rebuild the stage to prove that reuse does not depend on process memory,
	// the old Sandbox, parser, artifact encoder, or current Registry settings.
	stage = runner.BaselineStage{Repository: BaselineStore{DB: db}}
	for range 2 {
		e.Baseline = nil
		if err := stage.Run(ctx, e); err != nil || e.Baseline == nil || !reflect.DeepEqual(*e.Baseline, original) {
			t.Fatalf("continue did not reuse original baseline: %+v %v", e.Baseline, err)
		}
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM test_runs WHERE kind = 'baseline'`).Scan(&rows); err != nil || rows != 1 || *calls != 1 {
		t.Fatalf("continue reacquired baseline: rows=%d sandbox calls=%d err=%v", rows, *calls, err)
	}
}

func TestBaselineFreshWorktreeDoesNotReusePreviousBaseline(t *testing.T) {
	db, stage, e, calls := baselineFixture(t)
	if err := stage.Run(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if _, err := insertExecution(db, "execution-2", "task-001", 2, "pending"); err != nil {
		t.Fatal(err)
	}
	mustTaskExec(t, db, `INSERT INTO worktrees SELECT 'worktree-2', project_id, task_id, runner_id,
		'/worktrees/task-001-r2', 'evaloom/task-001-r2', base_sha, status, setup_status, copied_files, created_at, cleaned_at FROM worktrees`)
	mustTaskExec(t, db, `UPDATE executions SET mode = 'fresh', worktree_id = 'worktree-2', base_sha = ?, runner_id = 'mac-mini' WHERE id = 'execution-2'`, e.BaseSHA)
	e.Claim.Execution.ID, e.WorktreeID, e.WorktreePath = "execution-2", "worktree-2", "/worktrees/task-001-r2"
	if err := stage.Run(context.Background(), e); err != nil || *calls != 2 {
		t.Fatalf("fresh worktree failed to acquire its own baseline: calls=%d err=%v", *calls, err)
	}
}

func TestBaselineInvalidReportPersistsNullCountsAndIsNotReacquired(t *testing.T) {
	for _, body := range []string{"", "<testsuite>"} {
		t.Run(body, func(t *testing.T) {
			db, stage, e, _ := baselineFixture(t)
			calls := 0
			stage.Sandbox = baselineSandboxFunc(func(context.Context, *runner.Execution, runner.BaselineTestRequest) (runner.BaselineTestOutput, error) {
				calls++
				out := runner.BaselineTestOutput{TargetSHA: e.BaseSHA, ExitCode: new(0)}
				if body != "" {
					out.Report = io.NopCloser(strings.NewReader(body))
				}
				return out, nil
			})
			for range 2 {
				if err := stage.Run(context.Background(), e); !errors.Is(err, runner.ErrBaselineUnverifiable) {
					t.Fatalf("missing/invalid report accepted: %v", err)
				}
			}
			var unknown bool
			if err := db.QueryRow(`SELECT total IS NULL AND passed IS NULL AND failed IS NULL AND skipped IS NULL AND test_ids_artifact_id IS NULL FROM test_runs`).Scan(&unknown); err != nil || !unknown || calls != 1 {
				t.Fatalf("unverifiable baseline: unknown=%v calls=%d err=%v", unknown, calls, err)
			}
		})
	}
}

func TestBaselineCountOnlyAndFailingTestsAreRecorded(t *testing.T) {
	for _, exit := range []int{0, 1} {
		db, stage, e, _ := baselineFixture(t)
		stage.Sandbox = baselineSandboxFunc(func(context.Context, *runner.Execution, runner.BaselineTestRequest) (runner.BaselineTestOutput, error) {
			return runner.BaselineTestOutput{TargetSHA: e.BaseSHA, ExitCode: &exit, Report: io.NopCloser(strings.NewReader(
				`<testsuite tests="4" failures="1" skipped="1"/>`))}, nil
		})
		for range 2 { // Both acquisition and reuse continue after measured failure (Q39).
			if err := stage.Run(context.Background(), e); err != nil {
				t.Fatalf("measured baseline failure stopped work: %v", err)
			}
		}
		var total, failed, skipped, gotExit int
		var ids sql.NullString
		if err := db.QueryRow(`SELECT total, failed, skipped, exit_code, test_ids_artifact_id FROM test_runs`).Scan(&total, &failed, &skipped, &gotExit, &ids); err != nil || total != 4 || failed != 1 || skipped != 1 || gotExit != exit || ids.Valid {
			t.Fatalf("count-only failure lost: total=%d failed=%d skipped=%d exit=%d ids=%v err=%v", total, failed, skipped, gotExit, ids, err)
		}
	}
}

func TestBaselineRejectsWrongSHAAndSandboxFailureWithoutSaving(t *testing.T) {
	for _, cause := range []error{nil, context.DeadlineExceeded, context.Canceled, &sandbox.Error{Op: "create failed"}} {
		db, stage, e, _ := baselineFixture(t)
		stage.Sandbox = baselineSandboxFunc(func(context.Context, *runner.Execution, runner.BaselineTestRequest) (runner.BaselineTestOutput, error) {
			return runner.BaselineTestOutput{TargetSHA: strings.Repeat("b", 40), ExitCode: new(0)}, cause
		})
		err := stage.Run(context.Background(), e)
		if err == nil || cause != nil && !errors.Is(err, cause) {
			t.Fatalf("lost sandbox failure or accepted wrong SHA: %v", err)
		}
		var rows int
		if err := db.QueryRow(`SELECT count(*) FROM test_runs`).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("invalid baseline saved: rows=%d err=%v", rows, err)
		}
	}
}

func TestBaselineStorageFailureStopsBeforeHarness(t *testing.T) {
	db, stage, e, _ := baselineFixture(t)
	mustTaskExec(t, db, `CREATE TRIGGER reject_baseline BEFORE INSERT ON test_runs BEGIN SELECT RAISE(ABORT, 'storage unavailable'); END`)
	if err := stage.Run(context.Background(), e); err == nil || e.Baseline != nil {
		t.Fatalf("baseline falsely acknowledged despite database failure: baseline=%+v err=%v", e.Baseline, err)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM test_runs`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed write left baseline: %d %v", rows, err)
	}
}

func TestBaselineStoreRejectsScopeChangesAndOverwrites(t *testing.T) {
	db, stage, e, _ := baselineFixture(t)
	ctx := context.Background()
	if err := stage.Run(ctx, e); err != nil {
		t.Fatal(err)
	}
	s := BaselineStore{DB: db}
	if err := s.SaveBaseline(ctx, e.WorktreeID, *e.Baseline); err != nil {
		t.Fatalf("identical retry rejected: %v", err)
	}
	replacement := *e.Baseline
	replacement.Total = new(99)
	if err := s.SaveBaseline(ctx, e.WorktreeID, replacement); !errors.Is(err, ErrBaselineExists) {
		t.Fatalf("baseline overwritten: %v", err)
	}
	for _, change := range []string{
		`UPDATE executions SET base_sha = '` + strings.Repeat("b", 40) + `'`,
		`UPDATE executions SET runner_id = NULL`,
		`UPDATE worktrees SET status = 'cleaned'`,
	} {
		t.Run(change, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(change); err != nil {
				t.Fatal(err)
			}
			if _, err := baselineScope(ctx, tx, e.Claim.Execution.ID, e.WorktreeID); !errors.Is(err, ErrBaselineScope) {
				t.Fatalf("invalid scope accepted: %v", err)
			}
		})
	}
	if _, err := s.FindBaseline(ctx, e.Claim.Execution.ID, "different-worktree"); !errors.Is(err, ErrBaselineScope) {
		t.Fatalf("wrong worktree accepted: %v", err)
	}
}

func TestBaselineStorePreservesUnknownMeasurements(t *testing.T) {
	db, _, e, _ := baselineFixture(t)
	now := time.Now().UTC()
	run := model.TestRun{ID: "unknown", ExecutionID: e.Claim.Execution.ID, Kind: "baseline", TargetSHA: e.BaseSHA,
		Command: "make test", StartedAt: now, FinishedAt: now, TimedOut: true}
	s := BaselineStore{DB: db}
	if err := s.SaveBaseline(context.Background(), e.WorktreeID, run); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindBaseline(context.Background(), e.Claim.Execution.ID, e.WorktreeID)
	if err != nil || got == nil || !reflect.DeepEqual(*got, run) {
		t.Fatalf("unknown measurements changed: %+v %v", got, err)
	}
}

func TestBaselineCountOnlySuccessPreservesCountsWithoutIDArtifact(t *testing.T) {
	db, stage, e, _ := baselineFixture(t)
	stage.Sandbox = baselineSandboxFunc(func(context.Context, *runner.Execution, runner.BaselineTestRequest) (runner.BaselineTestOutput, error) {
		return runner.BaselineTestOutput{TargetSHA: e.BaseSHA, ExitCode: new(0), Report: io.NopCloser(strings.NewReader(
			`<testsuite tests="5" skipped="2"/>`))}, nil
	})
	stage.RecordIDs = nil
	if err := stage.Run(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	var total, passed, skipped int
	var ids sql.NullString
	if err := db.QueryRow(`SELECT total, passed, skipped, test_ids_artifact_id FROM test_runs`).Scan(&total, &passed, &skipped, &ids); err != nil || total != 5 || passed != 3 || skipped != 2 || ids.Valid {
		t.Fatalf("count-only baseline: total=%d passed=%d skipped=%d ids=%v err=%v", total, passed, skipped, ids, err)
	}
}

func TestBaselineLookupFailureDoesNotRunTests(t *testing.T) {
	db, stage, e, calls := baselineFixture(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stage.Run(context.Background(), e); err == nil || *calls != 0 {
		t.Fatalf("failed lookup triggered acquisition: calls=%d err=%v", *calls, err)
	}
}

func TestBaselineUnsetCommandDefersToGate2(t *testing.T) {
	db, stage, e, calls := baselineFixture(t)
	e.Claim.Project.TestCommand = nil
	stage.Sandbox, stage.Parse, stage.RecordIDs = nil, nil, nil
	if err := stage.Run(context.Background(), e); err != nil || *calls != 0 {
		t.Fatalf("unset command was executed or classified before Gate 2: calls=%d err=%v", *calls, err)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM test_runs`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("unset command produced a baseline: rows=%d err=%v", rows, err)
	}
}

func TestBaselineIDStorageFailureDoesNotAcknowledgeBaseline(t *testing.T) {
	db, stage, e, _ := baselineFixture(t)
	failure := errors.New("artifact storage unavailable")
	stage.RecordIDs = func(context.Context, string, []string) (string, error) { return "", failure }
	if err := stage.Run(context.Background(), e); !errors.Is(err, failure) || e.Baseline != nil {
		t.Fatalf("ID storage failure was ignored: baseline=%+v err=%v", e.Baseline, err)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM test_runs`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("ID storage failure produced a baseline: rows=%d err=%v", rows, err)
	}
}
