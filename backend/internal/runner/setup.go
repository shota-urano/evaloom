package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// SetupRepository is a Control Plane boundary, never a Runner DB connection.
// SaveSetup must atomically save test_runs and worktrees.setup_status, and ACK
// identical retries without inserting another measurement.
type SetupRepository interface {
	SetupSucceeded(context.Context, string, string) (bool, error)
	SaveSetup(context.Context, string, model.TestRun, bool) error
}

type SetupRequest struct {
	WorktreeID, WorktreePath, TargetSHA, Command string
}

// SetupStage implements StageSetup after the worktree/copy stages have saved
// the worktree association. Prepare constructs an isolated runtime with the
// supplied output writers, trusted mounts, allowlisted environment/network,
// resource limits and command argv. It must not execute the Registry command.
// Command interpretation is supplied by the caller, as for SandboxBaseline.
type SetupStage struct {
	Repository SetupRepository
	NewLog     func(executionID string) (*Log, error)
	Prepare    func(context.Context, SetupRequest, io.Writer, io.Writer) (sandbox.Sandbox, sandbox.SandboxSpec, []string, error)
}

func (stage SetupStage) Run(ctx context.Context, e *Execution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil || e.control == nil || e.status != model.ExecutionPreparing {
		return fmt.Errorf("runner: controlled preparing execution is required for setup")
	}
	command := e.Claim.Project.SetupCommand
	if command == nil || strings.TrimSpace(*command) == "" {
		return nil
	}
	if e.WorktreeID == "" || e.WorktreePath == "" || e.BaseSHA == "" {
		return fmt.Errorf("runner: setup repository and recorded worktree are required")
	}
	if stage.Repository == nil {
		stage.Repository = remoteSetupRepository{execution: e}
	}
	succeeded, err := stage.Repository.SetupSucceeded(ctx, e.Claim.Execution.ID, e.WorktreeID)
	if err != nil {
		return fmt.Errorf("runner: read worktree setup status failed")
	}
	if !e.newWorktreeRequired() && succeeded {
		return nil
	}
	if stage.Prepare == nil || stage.NewLog == nil {
		return fmt.Errorf("runner: setup sandbox and log configuration are required")
	}
	ctx, cancel := e.stageContext(ctx, StageSetup)
	defer cancel()
	now := e.now
	if now == nil {
		now = time.Now
	}
	run := model.TestRun{ID: ulid.Make().String(), ExecutionID: e.Claim.Execution.ID,
		Kind: "setup", TargetSHA: e.BaseSHA, Command: *command, StartedAt: now().UTC()}
	log, err := stage.NewLog(run.ExecutionID)
	if err != nil {
		return fmt.Errorf("runner: create setup log failed")
	}
	if log == nil || log.stream != "setup" || log.executionID != run.ExecutionID {
		if log != nil {
			_ = log.Finish(false)
		}
		return fmt.Errorf("runner: invalid setup log")
	}
	// Drain both streams concurrently through the existing redactor. Delivery
	// never blocks collection and uses the independent report context.
	delivery := make(chan error, 1)
	go func() { delivery <- log.Deliver(e.control.ReportContext) }()
	outReader, outWriter := io.Pipe()
	errReader, errWriter := io.Pipe()
	reads := make(chan error, 2)
	for _, reader := range []*io.PipeReader{outReader, errReader} {
		go func(reader *io.PipeReader) {
			readErr := log.Read(reader)
			_ = reader.CloseWithError(readErr)
			reads <- readErr
		}(reader)
	}
	runtime, spec, argv, runErr := stage.Prepare(ctx, SetupRequest{
		WorktreeID: e.WorktreeID, WorktreePath: e.WorktreePath, TargetSHA: e.BaseSHA, Command: run.Command,
	}, outWriter, errWriter)
	if ctx.Err() != nil {
		runErr = ctx.Err()
	}
	if runErr == nil {
		if runtime == nil || spec.Kind != sandbox.TestSetup || spec.Worktree != e.WorktreePath ||
			spec.ConfigDir != "" || len(argv) == 0 ||
			spec.Network != "" && spec.Network != sandbox.NetworkNone && spec.Network != sandbox.NetworkSetup {
			runErr = fmt.Errorf("runner: invalid setup sandbox specification")
		} else {
			var process sandbox.Process
			process, runErr = runtime.Run(ctx, spec, argv)
			if runErr == nil {
				if process == nil {
					runErr = fmt.Errorf("runner: setup sandbox returned no process")
				} else {
					runErr = e.WaitProcess(ctx, process)
					if code, known := process.ExitCode(); known {
						run.ExitCode = &code
					}
				}
			}
		}
	}
	// WaitProcess confirms the whole sandbox has stopped before EOF and save.
	_ = outWriter.Close()
	_ = errWriter.Close()
	logErr := errors.Join(<-reads, <-reads, log.Finish(false))
	run.FinishedAt = now().UTC()
	status, class := setupOutcome(ctx, runErr, run.ExitCode)
	run.TimedOut = status == model.ExecutionTimedOut
	// Cancellation must not discard the failed/timed-out measurement or logs.
	saveErr := stage.Repository.SaveSetup(e.control.ReportContext, e.WorktreeID, run, status == "")
	if saveErr == nil && status != "" {
		saveErr = e.reportResult(status, class, nil, "セットアップを完了できない")
	}
	logErr = errors.Join(logErr, <-delivery)
	if saveErr != nil || logErr != nil {
		return fmt.Errorf("runner: persist setup result or log failed")
	}
	switch status {
	case model.ExecutionTimedOut:
		return context.DeadlineExceeded
	case model.ExecutionCancelled:
		return context.Canceled
	case model.ExecutionFailed:
		return fmt.Errorf("runner: setup failed")
	}
	return nil
}

// The claim supplies the saved status; writes use the authenticated result
// reporter, including its transport retry policy. No Runner DB is required.
type remoteSetupRepository struct{ execution *Execution }

func (r remoteSetupRepository) SetupSucceeded(ctx context.Context, executionID, worktreeID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	e := r.execution
	if executionID != e.Claim.Execution.ID || worktreeID != e.WorktreeID {
		return false, fmt.Errorf("runner: invalid setup lookup scope")
	}
	if e.newWorktreeRequired() {
		return false, nil
	}
	w := e.Claim.Worktree
	if w == nil || w.ID != worktreeID || w.Path != e.WorktreePath || w.BaseSHA != e.BaseSHA {
		return false, fmt.Errorf("runner: missing or mismatched setup snapshot")
	}
	switch w.SetupStatus {
	case "succeeded":
		return true, nil
	case "pending", "failed":
		return false, nil
	default:
		return false, fmt.Errorf("runner: missing or invalid setup status")
	}
}

func (r remoteSetupRepository) SaveSetup(ctx context.Context, worktreeID string, run model.TestRun, succeeded bool) error {
	e := r.execution
	if worktreeID != e.WorktreeID || run.ExecutionID != e.Claim.Execution.ID {
		return fmt.Errorf("runner: invalid setup save scope")
	}
	status := "failed"
	if succeeded {
		status = "succeeded"
	}
	return e.control.Reporter.ReportResult(ctx, run.ExecutionID, ExecutionResult{
		FromStatus: model.ExecutionPreparing, Status: model.ExecutionPreparing,
		WorktreeID: worktreeID, SetupStatus: status, TestRuns: []model.TestRun{run},
	})
}

func setupOutcome(ctx context.Context, err error, code *int) (model.ExecutionStatus, model.ErrorClass) {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return model.ExecutionTimedOut, model.ErrorTimeout
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		return model.ExecutionCancelled, model.ErrorCancelled
	}
	var exit *sandbox.ExitError
	if err != nil && !errors.As(err, &exit) || code == nil {
		return model.ExecutionFailed, model.ErrorSandbox
	}
	if *code != 0 || err != nil {
		return model.ExecutionFailed, model.ErrorConfig
	}
	return "", ""
}
