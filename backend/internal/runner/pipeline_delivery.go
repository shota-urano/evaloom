package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

// PipelineDependenciesForClaim is shared by the executable and integration
// tests. All persistence goes through the authenticated Control Plane API.
// Only runtime configuration and the Adapter registry may be replaced in tests.
func (r *Runner) PipelineDependenciesForClaim(config Config, claim Claim) PipelineDependencies {
	d := &pipelineDelivery{runner: r, config: config, claim: claim, audit: &ResultAudit{}}
	return PipelineDependencies{
		Copies: d, Reuse: d, Audit: d, delivery: d,
		FreshNumber: func(_ context.Context, c Claim) (int, error) {
			if c.Execution.FreshNumber == nil || *c.Execution.FreshNumber < 1 {
				return 0, ErrPipelineInput
			}
			return *c.Execution.FreshNumber, nil
		},
		Policy: func(_ context.Context, c Claim) (PipelinePolicy, error) {
			variable := "CODEX_HOME"
			if c.Profile.Adapter == "claude_code" {
				variable = "CLAUDE_CONFIG_DIR"
			}
			limits := config.Container.ResourceLimits(0)
			return PipelinePolicy{CPU: limits.CPU, MemoryBytes: limits.MemoryBytes, Shell: "/bin/sh", JUnitFormat: "junit_xml",
				HarnessRepository: c.Profile.HarnessRepoPath, ConfigVariable: variable, SetupDestinations: append([]string(nil), c.SetupAllowedHosts...)}, nil
		},
		Publication: func(_ context.Context, _ Claim) (publisher.PublishConfig, error) {
			return publisher.PublishConfig{Recorder: d, Push: publisher.PushConfig{RecordRemoteMismatch: func(_ context.Context, verified, observed string) error {
				d.audit.Action = "publish.remote_mismatch"
				d.audit.Detail = map[string]string{"branch": d.branch, "verifiedSha": verified, "observedSha": observed}
				return nil
			}}}, nil
		},
	}
}

type pipelineDelivery struct {
	runner *Runner
	config Config
	claim  Claim
	audit  *ResultAudit
	branch string
}

func (d *pipelineDelivery) RecordCopiedWorktree(ctx context.Context, id string, w model.PreparedWorktree) (string, error) {
	worktreeID := ulid.Make().String()
	err := d.runner.ReportResult(ctx, id, ExecutionResult{FromStatus: model.ExecutionPreparing, Status: model.ExecutionPreparing,
		Worktree: &ResultWorktree{Action: "created", ID: worktreeID, Path: w.Path, BranchName: w.BranchName, BaseSHA: w.BaseSHA, CopiedFiles: &w.CopiedFiles}})
	return worktreeID, err
}
func (d *pipelineDelivery) WarnMissingCopy(ctx context.Context, id, path string) error {
	index := -1
	for i, p := range d.claim.Project.CopyAllowlist {
		if p == path {
			index = i
			break
		}
	}
	if index < 0 {
		return ErrPipelineInput
	}
	payload, _ := json.Marshal(struct {
		Events []logEvent `json:"events"`
	}{[]logEvent{{ID: "worktree-copy-missing/" + strconv.Itoa(index), Stream: "runner", Text: "コピー元が無い: " + path}}})
	log := &Log{runner: d.runner, executionID: id}
	return log.post(ctx, "events", bytes.NewReader(payload), "application/json")
}
func (d *pipelineDelivery) RecordWorktreePathRejection(_ context.Context, _ string) error {
	path := filepath.Join(d.config.WorktreeRoot, d.claim.Execution.TaskID)
	if d.claim.Worktree != nil {
		path = d.claim.Worktree.Path
	} else if d.claim.Execution.Mode == "fresh" && d.claim.Execution.FreshNumber != nil {
		path += "-r" + strconv.Itoa(*d.claim.Execution.FreshNumber)
	}
	real, err := resolvePipelinePath(path)
	if err != nil {
		return err
	}
	d.audit.Action = "worktree.path_rejected"
	d.audit.Detail = map[string]string{"path": real}
	return nil
}
func resolvePipelinePath(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("runner: cannot resolve worktree path")
	}
	// Resolve an absent suffix without following a dangling link.
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("runner: dangling worktree path")
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", fmt.Errorf("runner: cannot resolve worktree path")
	}
	real, err = resolvePipelinePath(parent)
	return filepath.Join(real, filepath.Base(path)), err
}
func (d *pipelineDelivery) LoadWorktreeReuse(_ context.Context, id, worktreeID string) (model.WorktreeReuseState, error) {
	w := d.claim.Worktree
	if id != d.claim.Execution.ID || w == nil || w.ID != worktreeID {
		return model.WorktreeReuseState{}, ErrPipelineInput
	}
	return model.WorktreeReuseState{Status: w.Status, ExpectedHeadSHA: w.ExpectedHeadSHA, PreparationComplete: w.PreparationComplete}, nil
}
func (d *pipelineDelivery) BeginWorktreeRestore(ctx context.Context, id, worktreeID string) error {
	return d.runner.ReportResult(ctx, id, ExecutionResult{FromStatus: model.ExecutionPreparing, Status: model.ExecutionPreparing,
		Worktree: &ResultWorktree{Action: "restore_started", ID: worktreeID}})
}
func (d *pipelineDelivery) RecordRestoredCopies(ctx context.Context, id, worktreeID string, w model.PreparedWorktree) error {
	return d.runner.ReportResult(ctx, id, ExecutionResult{FromStatus: model.ExecutionPreparing, Status: model.ExecutionPreparing,
		Worktree: &ResultWorktree{Action: "restored", ID: worktreeID, Path: w.Path, BranchName: w.BranchName, BaseSHA: w.BaseSHA, CopiedFiles: &w.CopiedFiles}})
}
func (d *pipelineDelivery) RecordPushed(ctx context.Context, sha, branch string) error {
	return d.runner.ReportResult(ctx, d.claim.Execution.ID, ExecutionResult{FromStatus: model.ExecutionPublishing, Status: model.ExecutionPublishing, PushedSHA: sha, BranchName: branch})
}
func (d *pipelineDelivery) RecordPR(ctx context.Context, pr publisher.PullRequest, created bool) error {
	return d.runner.ReportResult(ctx, d.claim.Execution.ID, ExecutionResult{FromStatus: model.ExecutionPublishing, Status: model.ExecutionPublishing, PRNumber: &pr.Number, PRURL: pr.URL, PRCreated: &created})
}
func (p *claimPipeline) readSession(ctx context.Context, e *Execution) (*publisher.ReadSession, error) {
	reader, err := publisher.NewGitHubAppReader(p.runner.client, e.Claim.Project.GitRemoteURL, func(ctx context.Context) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return os.ReadFile(p.config.GitHubAppPrivateKeyFile)
	})
	if err != nil {
		return nil, err
	}
	return reader.ForStage(ctx, func(values ...string) { e.logSecrets = append(e.logSecrets, values...) })
}

func (p *claimPipeline) previousClaimPRs(ctx context.Context, e *Execution) error {
	if e.Claim.PullRequests == nil {
		return ErrPipelineInput
	}
	branch := "evaloom/" + e.Claim.Execution.TaskID + "-" + e.Claim.Task.ShortName
	if w := e.Claim.Worktree; w != nil {
		branch = w.BranchName
	}
	selected := []ClaimPullRequest{}
	for _, pr := range e.Claim.PullRequests {
		if e.Claim.Execution.Mode == "fresh" {
			selected = append(selected, pr)
		} else if pr.BranchName != nil && *pr.BranchName == branch {
			if len(selected) == 0 || pr.Seq > selected[0].Seq {
				selected = []ClaimPullRequest{pr}
			}
		}
	}
	if len(selected) == 0 {
		return nil
	}
	session, err := p.readSession(ctx, e)
	if err != nil {
		return err
	}
	var openHead string
	for _, pr := range selected {
		state, head, err := session.ObservePullRequest(ctx, e.Claim.Project.GitRemoteURL, pr.Number)
		if err != nil {
			return err
		}
		if state == publisher.PRMerged {
			return e.reportResult(model.ExecutionFailed, model.ErrorConfig, nil, "pr_merged")
		}
		if state == publisher.PRClosed && e.Claim.Execution.Mode == "continue" {
			return e.reportResult(model.ExecutionFailed, model.ErrorConfig, nil, "pr_closed")
		}
		if state == publisher.PROpen && e.Claim.Execution.Mode == "fresh" && openHead == "" {
			openHead = head
		}
	}
	if openHead != "" {
		return e.reportBlock(model.ExecutionBlocked, "", &ResultBlock{Cause: model.BlockGitState, HeadSHA: openHead})
	}
	return nil
}

// Resume starts at Gate 3/publish with the claim's own saved measurements. It
// never executes setup, baseline or the harness again, and checks the approved
// HEAD before any publication. Restoration requires preparation receipts, so
// an unavailable directory cannot be silently recreated at a later stage.
func (p *claimPipeline) prepareResume(ctx context.Context, e *Execution) error {
	w, b := e.Claim.Worktree, e.Claim.Execution.ResumeBlock
	if w == nil || b == nil || b.DecidedAt == nil || b.HeadSHA == "" || b.HeadSHA != e.Claim.Execution.ResumeHeadSHA {
		return ErrPipelineInput
	}
	if p.config.HarnessAuthFiles[e.Claim.Profile.HarnessID] == "" {
		return ErrPipelineInput
	}
	real, err := resolvePipelinePath(w.Path)
	root, rootErr := filepath.EvalSymlinks(p.config.WorktreeRoot)
	if err != nil || rootErr != nil || filepath.Dir(real) != root {
		if p.deps.Audit != nil {
			_ = p.deps.Audit.RecordWorktreePathRejection(e.control.ReportContext, e.Claim.Execution.ID)
		}
		return ErrPipelineInput
	}
	e.WorktreeID, e.WorktreePath, e.BaseSHA, e.BranchName = w.ID, w.Path, w.BaseSHA, w.BranchName
	e.CopiedFiles = w.CopiedFiles
	e.HarnessHeadSHA, e.VerifiedSHA, e.VerifyRun = e.Claim.Execution.HarnessHeadSHA, e.Claim.Execution.VerifiedSHA, e.Claim.Execution.VerifyRun
	// Reuse validates the Git directory/branch and the unchanged approved tree.
	// Returning an error before BeforeRecreate prevents an unrecorded restore.
	reused, err := (worktree.Repositories{WorktreeRoot: p.config.WorktreeRoot}).Reuse(ctx, worktree.ReuseRequest{
		ProjectID: e.Claim.Project.ID, RepositoryURL: e.Claim.Project.GitRemoteURL, Path: w.Path, BranchName: w.BranchName, BaseSHA: w.BaseSHA,
		ExpectedHeadSHA: b.HeadSHA, CopiedFiles: w.CopiedFiles, BeforeRecreate: func(context.Context) error { return ErrPipelineInput },
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, worktree.ErrWorktreePath) {
		if p.deps.Audit != nil {
			if auditErr := p.deps.Audit.RecordWorktreePathRejection(e.control.ReportContext, e.Claim.Execution.ID); auditErr != nil {
				return errors.Join(ErrPipelineInput, auditErr)
			}
		}
		return ErrPipelineInput
	}
	if err != nil {
		head := reused.HeadSHA
		if head == "" {
			return errors.Join(fmt.Errorf("runner: resume worktree check stopped execution"), e.reportResult(model.ExecutionBlocked, "",
				&ResultBlock{Cause: model.BlockGitState, HeadSHA: b.HeadSHA}, "現在のHEADを取得できない。記録したSHAは続行判断時のSHAであり、現在のHEADは未確認"))
		}
		return errors.Join(fmt.Errorf("runner: resume worktree check stopped execution"), e.reportBlock(model.ExecutionBlocked, "", &ResultBlock{Cause: model.BlockGitState, HeadSHA: head}))
	}
	if err := json.Unmarshal(e.Claim.Profile.Definition, &p.definition); err != nil {
		return err
	}
	summary := "作業概要を取得できなかった"
	if e.Claim.Execution.ResumeHarnessSummary != nil {
		summary = *e.Claim.Execution.ResumeHarnessSummary
	}
	e.HarnessResult = &adapter.HarnessResult{Summary: summary}
	p.gates = append([]GateResult(nil), e.Claim.Execution.ResumeGateResults...)
	return nil
}

func (p *claimPipeline) harnessHead(ctx context.Context, e *Execution) error {
	if err := p.runner.HarnessResultHandler()(ctx, e); err != nil {
		return err
	}
	data, err := json.Marshal(e.HarnessResult)
	if err != nil {
		return err
	}
	directory := filepath.Join(p.config.DataDir, "artifacts")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	id := ulid.Make().String()
	path := filepath.Join(directory, id+".json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr, closeErr := file.Sync(), file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return fmt.Errorf("runner: save harness result failed")
	}
	return p.runner.deliverArtifact(ctx, LogArtifact{ID: id, ExecutionID: e.Claim.Execution.ID, Kind: "harness_output", Path: path})
}
