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
	"github.com/shota-urano/evaloom/backend/internal/safegit"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

type Gate2Config struct {
	// Use the same runtime as stage 7.
	BaselineConfig
	// The specs provide no Runner-authorized read endpoint for saved test_ids.
	// This boundary must read trusted artifact storage, never the worktree.
	// It is needed only after reusing an ID-bearing baseline (e.g. continue).
	LoadTestIDs func(context.Context, model.TestRun) ([]string, error)
}

var ErrGate2Stopped = errors.New("runner: Gate 2 stopped execution")

// NewGate2Executor connects stages 7, 9 and 10 to the fixed execution table.
// Other stages, including Gate 3 and publishing, remain required boundaries.
// Both test runs use the JUnit ID convention from spec 10, 3.3.2.
func (runner *Runner) NewGate2Executor(handlers map[StageID]StageFunc, control ExecutionControl, config Gate2Config) (*Executor, error) {
	baseline, err := runner.BaselineHandler(config.BaselineConfig)
	if err != nil {
		return nil, err
	}
	verify, err := runner.Gate2Handler(config)
	if err != nil {
		return nil, err
	}
	wired := make(map[StageID]StageFunc, len(handlers)+3)
	for id, handler := range handlers {
		wired[id] = handler
	}
	wired[StageTestBaseline], wired[StageGate2] = baseline, verify
	wired[StageHarnessHead] = func(ctx context.Context, e *Execution) error {
		if e.WorktreePath == "" {
			return fmt.Errorf("runner: harness head requires prepared worktree")
		}
		body, err := safegit.Output(ctx, e.WorktreePath, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("runner: cannot read harness head")
		}
		e.HarnessHeadSHA = strings.TrimSpace(string(body))
		return nil
	}
	return NewExecutorWithControl(wired, control)
}

// Gate2Handler is installed at StageGate2 in the product execution table.
// The harness-head handler supplies HarnessHeadSHA; Gate 3 consumes VerifyRun
// and VerifiedSHA. Results join the next transition, as existing gateResults do.
func (runner *Runner) Gate2Handler(config Gate2Config) (StageFunc, error) {
	if _, err := runner.BaselineHandler(config.BaselineConfig); err != nil {
		return nil, err
	}
	return func(ctx context.Context, e *Execution) error {
		if e == nil || e.control == nil || e.status != model.ExecutionVerifying || e.HarnessHeadSHA == "" || e.BaseSHA == "" || e.WorktreeID == "" || e.WorktreePath == "" {
			return fmt.Errorf("runner: Gate 2 requires a controlled verifying execution and prepared worktree")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.VerifyRun != nil {
			return fmt.Errorf("runner: Gate 2 already executed")
		}
		e.VerifiedSHA = ""
		if e.Claim.Project.TestCommand == nil || strings.TrimSpace(*e.Claim.Project.TestCommand) == "" {
			e.gateResults = append(e.gateResults, GateResult{Gate: 2, Result: "failed", BaseSHA: e.BaseSHA, TargetSHA: e.HarnessHeadSHA})
			return errors.Join(ErrGate2Stopped, e.report(model.ExecutionFailed, model.ErrorConfig))
		}
		now := time.Now
		if e.now != nil {
			now = e.now
		}
		project := e.Claim.Project
		run := model.TestRun{ID: ulid.Make().String(), ExecutionID: e.Claim.Execution.ID, Kind: "verify", TargetSHA: e.HarnessHeadSHA, Command: *project.TestCommand, StartedAt: now().UTC()}
		request := BaselineTestRequest{Kind: sandbox.TestRun, WorktreeID: e.WorktreeID, WorktreePath: e.WorktreePath, TargetSHA: run.TargetSHA, Command: run.Command, ReportFormat: project.TestReportFormat, ReportPath: project.TestReportPath}
		launcher := newPreparedBaselineSandbox(config.BaselineConfig)
		// Missing/unsupported report configuration still executes the Registry
		// command once, then yields an unavailable report. Never read stale data.
		if _, err := baselineReportPath(request, config.JUnitFormat); err != nil {
			launcher.Prepare = func(context.Context, BaselineTestRequest) (sandbox.SandboxSpec, []string, error) {
				return sandbox.SandboxSpec{Kind: sandbox.TestRun, Worktree: e.WorktreePath, Network: sandbox.NetworkNone, Limits: config.Limits}, []string{config.Shell, "-c", run.Command}, nil
			}
			launcher.OpenReport = func(context.Context, BaselineTestRequest) (io.ReadCloser, error) { return nil, nil }
		}
		output, runErr := launcher.RunBaseline(ctx, e, request)
		if output.Report != nil {
			defer output.Report.Close()
		}
		run.ExitCode, run.FinishedAt = output.ExitCode, now().UTC()
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			run.TimedOut = true
		}
		if run.TimedOut || errors.Is(runErr, context.Canceled) || ctx.Err() != nil {
			e.VerifyRun, e.testRuns = &run, append(e.testRuns, run)
			return errors.Join(ctx.Err(), runErr) // Executor reports only after process termination.
		}
		// OpenReport failures retain measured exit/SHA and mean unverifiable,
		// not a Sandbox creation failure. The launcher never invents an exit.
		if runErr != nil && (output.TargetSHA != run.TargetSHA || output.ExitCode == nil) {
			e.VerifyRun, e.testRuns = &run, append(e.testRuns, run)
			e.gateResults = append(e.gateResults, GateResult{Gate: 2, Result: "failed", BaseSHA: e.BaseSHA, TargetSHA: run.TargetSHA})
			return errors.Join(ErrGate2Stopped, e.report(model.ExecutionFailed, model.ErrorSandbox))
		}
		var report *gate.TestReport
		if runErr == nil && output.TargetSHA == run.TargetSHA && output.Report != nil {
			if parsed, err := gate.ParseJUnitXML(output.Report); err == nil {
				report = &parsed
				run.Total, run.Passed, run.Failed, run.Skipped = &parsed.Total, &parsed.Passed, &parsed.Failed, &parsed.Skipped
				if parsed.IDsAvailable {
					id, err := runner.recordBaselineIDs(e.control.ReportContext, run.ExecutionID, parsed.TestIDs)
					if err != nil {
						return err
					}
					run.TestIDsArtifactID = &id
				}
			}
		}
		baseline := e.Baseline
		if baseline == nil && e.Claim.Worktree != nil {
			baseline = e.Claim.Worktree.Baseline
		}
		ids := e.BaselineIDs
		if baseline != nil && baseline.TargetSHA != e.BaseSHA {
			baseline = nil
		}
		if baseline != nil && baseline.TestIDsArtifactID != nil && ids == nil && config.LoadTestIDs != nil && report != nil && report.IDsAvailable && run.ExitCode != nil && *run.ExitCode == 0 && report.Failed == 0 {
			loaded, err := config.LoadTestIDs(ctx, *baseline)
			if err == nil {
				ids = loaded
			}
		}
		if ctx.Err() != nil {
			run.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
			e.VerifyRun, e.testRuns = &run, append(e.testRuns, run)
			return ctx.Err()
		}
		decision := gate.Verify(baseline, ids, run, report)
		findings := []model.Finding{}
		if decision.CountOnly {
			findings = append(findings, model.Finding{Rule: "count_only_check", Message: "件数による確認"})
		}
		switch decision.Cause {
		case model.BlockGate2MissingTests:
			if len(decision.MissingIDs) == 0 {
				findings = append(findings, model.Finding{Rule: "existing_test_not_run", Message: "実行数の減少、またはskip数の増加を検出した"})
			}
			for _, id := range decision.MissingIDs {
				findings = append(findings, model.Finding{Rule: "existing_test_not_run", Message: fmt.Sprintf("既存のテスト %q が実行されていない", id)})
			}
		case model.BlockGate2Unverifiable:
			findings = append(findings, model.Finding{Rule: "test_report_missing", Message: "テスト結果または基準値を照合できない"})
		}
		if decision.ErrorClass == model.ErrorTestFailed {
			findings = append(findings, model.Finding{Rule: "test_failed", Message: "テストが失敗した"})
		}
		e.VerifyRun, e.testRuns = &run, append(e.testRuns, run)
		e.gateResults = append(e.gateResults, GateResult{Gate: 2, Result: decision.Result, BaseSHA: e.BaseSHA, TargetSHA: run.TargetSHA, Findings: findings})
		if decision.Result == "pass" {
			e.VerifiedSHA = run.TargetSHA
			return nil
		}
		var block *ResultBlock
		if decision.Cause != "" {
			block = &ResultBlock{Cause: decision.Cause, HeadSHA: run.TargetSHA}
		}
		status := model.ExecutionFailed
		if block != nil {
			status = model.ExecutionBlocked
		}
		return errors.Join(ErrGate2Stopped, e.reportBlock(status, decision.ErrorClass, block))
	}, nil
}
