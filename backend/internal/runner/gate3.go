package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/gate"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

type Gate3Config struct {
	// Spec 10 does not define Finding rule names. Supply policy explicitly.
	MismatchRule string
	// Preparation owns restoring a cleaned worktree and copying its files.
	// Resume skips stage 3, so the same preparation boundary can be used here.
	PrepareResume StageFunc
}

var ErrGate3Stopped = errors.New("runner: Gate 3 stopped execution")

// NewVerificationExecutor wires both Gates into the product execution table.
// The remaining worktree, harness and publisher stages are supplied by their
// respective implementations, as in NewGate2Executor.
func (runner *Runner) NewVerificationExecutor(handlers map[StageID]StageFunc, control ExecutionControl, gate2 Gate2Config, gate3 Gate3Config) (*Executor, error) {
	handler, err := runner.Gate3Handler(gate3)
	if err != nil {
		return nil, err
	}
	wired := make(map[StageID]StageFunc, len(handlers)+1)
	for id, stage := range handlers {
		wired[id] = stage
	}
	wired[StageGate3] = handler
	return runner.NewGate2Executor(wired, control, gate2)
}

func (runner *Runner) Gate3Handler(config Gate3Config) (StageFunc, error) {
	if config.MismatchRule == "" {
		return nil, fmt.Errorf("runner: Gate 3 finding rule must be supplied explicitly")
	}
	return func(ctx context.Context, e *Execution) error {
		if e == nil || e.control == nil || e.status != model.ExecutionVerifying {
			return fmt.Errorf("runner: Gate 3 requires a controlled verifying execution")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Claim.Execution.ResumePoint != nil && *e.Claim.Execution.ResumePoint == "gate3" {
			// Only this Execution's saved measurement is accepted. A new
			// mode=continue execution must run Gate 2 again at its new HEAD.
			w := e.Claim.Worktree
			if w == nil {
				return fmt.Errorf("runner: Gate 3 resume requires saved worktree")
			}
			e.WorktreeID, e.WorktreePath, e.BaseSHA = w.ID, w.Path, w.BaseSHA
			e.CopiedFiles = w.CopiedFiles
			e.HarnessHeadSHA, e.VerifiedSHA, e.VerifyRun = e.Claim.Execution.HarnessHeadSHA, e.Claim.Execution.VerifiedSHA, e.Claim.Execution.VerifyRun
			if config.PrepareResume != nil {
				if err := config.PrepareResume(ctx, e); err != nil {
					return err
				}
			}
		}
		if e.WorktreeID == "" || e.WorktreePath == "" || e.BaseSHA == "" {
			return fmt.Errorf("runner: Gate 3 requires prepared worktree")
		}
		copied := e.CopiedFiles
		if copied == nil && e.Claim.Worktree != nil && e.WorktreeID == e.Claim.Worktree.ID {
			copied = e.Claim.Worktree.CopiedFiles
		}
		head, clean, err := gate.CheckWorktree(ctx, e.WorktreePath, copied)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && head == "" {
			return fmt.Errorf("runner: cannot inspect Gate 3 Git state")
		}
		result := GateResult{Gate: 3, Result: "pass", BaseSHA: e.BaseSHA, TargetSHA: head, Findings: []model.Finding{}}
		if err == nil && clean && gate.CheckCommit(e.Claim.Execution.ID, head, e.VerifiedSHA, e.VerifyRun) {
			e.gateResults = append(e.gateResults, result)
			return nil
		}
		result.Result = "blocked"
		message := "HEAD・作業ツリー・検証済みテストのSHAまたは実行数を確認できない"
		if err != nil {
			// Keep the observed HEAD and flush the pending Gate 2 measurement
			// even when status fails (for example, with a corrupt Git index).
			// Git's raw error may contain secrets; report only a safe reason.
			message = "Gitの作業ツリー状態を取得できない"
		}
		result.Findings = append(result.Findings, model.Finding{Rule: config.MismatchRule, Message: message})
		e.gateResults = append(e.gateResults, result)
		return errors.Join(ErrGate3Stopped, e.reportBlock(model.ExecutionBlocked, "", &ResultBlock{Cause: model.BlockGate3Mismatch, HeadSHA: head}))
	}, nil
}
