package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/gate"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// BaselineRepository is a Control Plane boundary, not permission for a Runner
// to open SQLite. FindBaseline returns nil only when this worktree has no run;
// it must search earlier Executions of the same worktree as well. SaveBaseline
// must durably acknowledge the immutable record before the harness can start.
// The production implementation uses the claim snapshot and result endpoint.
type BaselineRepository interface {
	FindBaseline(context.Context, string, string) (*model.TestRun, error)
	SaveBaseline(context.Context, string, model.TestRun) error
}

type BaselineTestRequest struct {
	Kind                                sandbox.SandboxKind
	WorktreeID, WorktreePath, TargetSHA string
	Command                             string
	ReportFormat, ReportPath            *string
}

type BaselineTestOutput struct {
	// TargetSHA must be the SHA independently checked by the trusted Sandbox
	// launcher, never a value read from the untrusted test report.
	TargetSHA string
	ExitCode  *int
	// Report must be from this invocation, not a stale setup/previous report.
	// The stage closes it, including on failure. nil means unavailable.
	Report io.ReadCloser
}

// BaselineTestSandbox must run the exact Registry command against TargetSHA
// in a test_run Sandbox after setup, with no host-execution fallback. It owns
// command-to-argv conversion, fresh report access and process supervision; it
// must honor ctx and return only after all Sandbox processes have stopped.
// Nonzero test exits are measurements in Output, not infrastructure errors.
type BaselineTestSandbox interface {
	RunBaseline(context.Context, *Execution, BaselineTestRequest) (BaselineTestOutput, error)
}

// BaselineStage implements stage 7 with parsing and artifact storage boundaries.
// RecordIDs must persist a test_ids artifact owned by executionID.
type BaselineStage struct {
	Repository BaselineRepository
	Sandbox    BaselineTestSandbox
	Parse      func(io.Reader) (gate.TestReport, error)
	RecordIDs  func(ctx context.Context, executionID string, ids []string) (string, error)
}

var ErrBaselineUnverifiable = errors.New("runner: baseline report unavailable or invalid")

// Run is installed as handlers[StageTestBaseline]. It reuses a saved record
// regardless of mode, including after interruption and continue retries. A
// missing record on continue is acquired only when HEAD is still the start SHA
// (the Sandbox boundary must enforce that precondition without resetting it).
func (stage BaselineStage) Run(ctx context.Context, execution *Execution) error {
	if stage.Repository == nil || execution == nil || execution.WorktreeID == "" || execution.BaseSHA == "" {
		return fmt.Errorf("runner: baseline repository and prepared worktree are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	previous, err := stage.Repository.FindBaseline(ctx, execution.Claim.Execution.ID, execution.WorktreeID)
	if err != nil {
		return err // A lookup failure must never trigger another test run.
	}
	if previous != nil {
		if previous.Kind != "baseline" || previous.TargetSHA != execution.BaseSHA {
			return fmt.Errorf("runner: saved baseline does not match the start commit")
		}
		execution.Baseline = previous
		return baselineOutcome(*previous)
	}
	project := execution.Claim.Project
	// Spec 05 assigns the unset-command config_error to Gate 2. Do not invent
	// an earlier baseline classification or run an empty command here.
	if project.TestCommand == nil || strings.TrimSpace(*project.TestCommand) == "" {
		return nil
	}
	if stage.Sandbox == nil || stage.Parse == nil || execution.WorktreePath == "" {
		return fmt.Errorf("runner: baseline sandbox, parser and worktree path are required")
	}
	now := time.Now
	if execution.now != nil {
		now = execution.now
	}
	run := model.TestRun{
		ID: ulid.Make().String(), ExecutionID: execution.Claim.Execution.ID,
		Kind: "baseline", TargetSHA: execution.BaseSHA, Command: *project.TestCommand,
		StartedAt: now().UTC(),
	}
	output, runErr := stage.Sandbox.RunBaseline(ctx, execution, BaselineTestRequest{
		Kind: sandbox.TestRun, WorktreeID: execution.WorktreeID, WorktreePath: execution.WorktreePath,
		TargetSHA: run.TargetSHA, Command: run.Command,
		ReportFormat: project.TestReportFormat, ReportPath: project.TestReportPath,
	})
	if output.Report != nil {
		defer output.Report.Close()
	}
	// Never label results from a different commit as a baseline. Infrastructure
	// failures retain their cause and are not mistaken for measured zero tests.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		return runErr
	}
	// A report-open failure after the measured process exited means the report
	// is unavailable; retain that measurement before applying Q39.
	if runErr != nil && (output.TargetSHA != run.TargetSHA || output.ExitCode == nil) {
		return runErr
	}
	if output.TargetSHA != run.TargetSHA {
		return fmt.Errorf("runner: baseline sandbox did not test the start commit")
	}
	run.ExitCode = output.ExitCode
	if runErr == nil && output.Report != nil {
		report, parseErr := stage.Parse(output.Report)
		if parseErr == nil {
			run.Total, run.Passed, run.Failed, run.Skipped = &report.Total, &report.Passed, &report.Failed, &report.Skipped
			if report.IDsAvailable {
				if stage.RecordIDs == nil {
					return fmt.Errorf("runner: baseline test ID recorder is required")
				}
				id, err := stage.RecordIDs(ctx, run.ExecutionID, report.TestIDs)
				if err != nil {
					return err
				}
				if id == "" {
					return fmt.Errorf("runner: baseline test IDs were not persisted")
				}
				run.TestIDsArtifactID = &id
				execution.BaselineIDs = append([]string{}, report.TestIDs...)
			}
		}
	}
	run.FinishedAt = now().UTC()
	if err := stage.Repository.SaveBaseline(ctx, execution.WorktreeID, run); err != nil {
		return err
	}
	execution.Baseline = &run
	return baselineOutcome(run)
}

// Q39 retains failing tests in the baseline. Only unavailable counts prevent
// work from starting; the controlled handler reports failed(config_error).
func baselineOutcome(run model.TestRun) error {
	if run.TimedOut {
		return context.DeadlineExceeded
	}
	if run.Total == nil || run.Skipped == nil {
		return ErrBaselineUnverifiable
	}
	return nil
}
