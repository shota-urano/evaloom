package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/claudecode"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/projectcontext"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
	"github.com/shota-urano/evaloom/backend/internal/registry"
	"github.com/shota-urano/evaloom/backend/internal/safegit"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

// These are process-control constants, not additional user configuration (05,
// 4.1). Workload deadlines always come from Project/defaultTimeouts.
const pipelineCancelGrace = 5 * time.Second
const pipelineControlTimeout = 5 * time.Second

// ErrPipelineInput means a required specification/delivery boundary has no
// implementation. Never substitute fabricated metadata or an acknowledged no-op.
var ErrPipelineInput = errors.New("runner: required pipeline input is unspecified")

// PipelinePolicy supplies values not defined by the configuration/claim schema.
// It is internal wiring, not a new JSON configuration or Control Plane API.
type PipelinePolicy struct {
	CPU                 float64
	MemoryBytes         int64
	Shell, JUnitFormat  string
	Env                 map[string]string
	SetupDestinations   []string
	HarnessDestinations []string
	HarnessRepository   string
	ConfigVariable      string
}

// PipelineDependencies isolates the missing delivery contracts called out in
// the existing components' READMEs. Production must supply real acknowledged
// implementations once those contracts are specified; nil fails closed.
type PipelineDependencies struct {
	Copies WorktreeCopyRecorder
	Reuse  WorktreeReuseRepository
	Audit  WorktreePathAuditor
	Read   worktree.ReadTransport
	Policy func(context.Context, Claim) (PipelinePolicy, error)
	// Seq includes continue executions and is NOT a fresh ordinal.
	FreshNumber func(context.Context, Claim) (int, error)
	PreviousPR  func(context.Context, Claim) (*int, error)
	// Publication must provide a durable recorder and mismatch auditor. The
	// runner supplies the trusted Git target/report and cancellation check.
	Publication func(context.Context, Claim) (publisher.PublishConfig, error)
	// Tests may replace only isolation/adapter boundaries. The nil production
	// path always uses Container and registers Codex/Claude Code by name.
	Sandbox  func(sandbox.ContainerConfig) (sandbox.Sandbox, error)
	Adapters *adapter.Registry
	delivery *pipelineDelivery
}

// ExecuteClaim connects all thirteen stages. Each call owns its configuration,
// adapter instances and logs; concurrent claims share no mutable stage data.
// reportCtx deliberately survives execution cancellation and must be owned by
// the caller. The function returns only after adapter writers and log delivery
// have stopped. Missing contracts are reported as config_error, never success.
func (r *Runner) ExecuteClaim(ctx, reportCtx context.Context, config Config, claim Claim, deps PipelineDependencies) (resultErr error) {
	if err := config.validate(); err != nil {
		return err
	}
	if reportCtx == nil {
		return fmt.Errorf("runner: report context is required")
	}
	p := &claimPipeline{runner: r, config: config, deps: deps}
	defer func() {
		if p.environment != nil {
			if err := p.environment.Close(); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("runner: configuration cleanup failed"))
			}
		}
	}()
	handlers := map[StageID]StageFunc{
		StageValidateProject: p.validate, StageCheckPreviousPR: p.previous,
		StagePrepareWorktree: p.prepare, StagePinContext: p.pin, StageReceiveContext: p.receive,
		StageSetup: p.setup, StageTestBaseline: p.baseline, StageHarness: func(ctx context.Context, e *Execution) error {
			err := p.harness(ctx, e)
			if p.environment != nil {
				err = errors.Join(err, p.environment.Close())
				p.environment = nil
			}
			return err
		},
		StageHarnessHead: p.harnessHead, StageGate2: p.verify, StageGate3: p.gate3,
		StagePublish: p.publish, StageComplete: func(context.Context, *Execution) error { return nil },
	}
	// Existing stage handlers report classified stops themselves. This wrapper
	// reports errors from preparation/delivery without leaving a claimed slot in
	// a nonterminal state. Do not expose component diagnostics or secret paths.
	for id, handler := range handlers {
		handlers[id] = func(ctx context.Context, e *Execution) error {
			if deps.delivery != nil {
				e.audit = deps.delivery.audit
			}
			err := handler(ctx, e)
			if err == nil && (e.status == model.ExecutionBlocked || e.status == model.ExecutionFailed) {
				err = fmt.Errorf("runner: stage stopped execution")
			}
			if err == nil || e.status == model.ExecutionFailed || e.status == model.ExecutionBlocked ||
				e.status == model.ExecutionCancelled || e.status == model.ExecutionTimedOut ||
				errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			class := model.ErrorConfig
			if id == StagePublish || id == StageCheckPreviousPR {
				class = model.ErrorPublishFailed
			}
			var classified interface{ ErrorClass() model.ErrorClass }
			if errors.As(err, &classified) {
				class = classified.ErrorClass()
			}
			if errors.Is(err, ErrPipelineInput) {
				class = model.ErrorConfig
			}
			return errors.Join(err, e.reportResult(model.ExecutionFailed, class, nil, "工程に必要な入力または処理を確認できない"))
		}
	}
	control := ExecutionControl{Timeouts: Timeouts{
		Setup:   time.Duration(config.DefaultTimeouts.SetupSec) * time.Second,
		Harness: time.Duration(config.DefaultTimeouts.HarnessSec) * time.Second,
		Test:    time.Duration(config.DefaultTimeouts.TestSec) * time.Second,
	}, CancelGrace: pipelineCancelGrace, Reporter: r, ReportContext: reportCtx, CheckCancellation: r.Heartbeat}
	executor, err := NewExecutorWithControl(handlers, control)
	if err != nil {
		return err
	}
	return executor.Execute(ctx, claim)
}

type claimPipeline struct {
	runner         *Runner
	config         Config
	deps           PipelineDependencies
	policy         PipelinePolicy
	definition     model.ProfileDefinition
	repository     string
	environment    *harnessenv.Environment
	requestContext adapter.RequestContext
	gates          []GateResult
}

func (p *claimPipeline) validate(ctx context.Context, e *Execution) error {
	if p.config.HarnessAuthFiles[e.Claim.Profile.HarnessID] == "" {
		return fmt.Errorf("runner: harness authentication is not configured")
	}
	input := registry.RepositoryInput{
		Path: e.Claim.Project.LocalRepoPath, URL: e.Claim.Project.GitRemoteURL,
		BaseBranch: e.Claim.Project.BaseBranch, WorktreeRoot: p.config.WorktreeRoot,
	}
	if p.deps.delivery != nil {
		session, err := p.readSession(ctx, e)
		if err != nil {
			return err
		}
		input.ReadRemote = session.Output
	}
	path, err := registry.ValidateRepository(ctx, input)
	if err == nil {
		e.Claim.Project.LocalRepoPath = path
	}
	return err
}

func (p *claimPipeline) github(e *Execution) (*publisher.GitHub, error) {
	return publisher.NewGitHubApp(p.runner.client, e.Claim.Project.GitRemoteURL, func(ctx context.Context) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return os.ReadFile(p.config.GitHubAppPrivateKeyFile)
	})
}

func (p *claimPipeline) previous(ctx context.Context, e *Execution) error {
	if p.deps.delivery != nil {
		return p.previousClaimPRs(ctx, e)
	}
	var number *int
	if p.deps.PreviousPR != nil {
		var err error
		number, err = p.deps.PreviousPR(ctx, e.Claim)
		if err != nil {
			return err
		}
	} else if e.Claim.Previous != nil && e.Claim.Previous.PRNumber != nil {
		number = e.Claim.Previous.PRNumber
	} else if e.Claim.Execution.Seq > 1 {
		// Previous contains only the immediate execution; nil is not evidence
		// that the task has no older PR, especially for fresh executions.
		return ErrPipelineInput
	}
	var reader publisher.PreviousPullRequest
	if number != nil {
		g, err := p.github(e)
		if err != nil {
			return err
		}
		reader = g
	}
	_, err := publisher.CheckPreviousPR(ctx, e.Claim.Project.GitRemoteURL, e.Claim.Execution.Mode, number, reader)
	var stop *publisher.RerunError
	if errors.As(err, &stop) && stop.Status == model.ExecutionFailed {
		return errors.Join(err, e.reportResult(stop.Status, stop.Class, nil, stop.Reason))
	}
	// An open/fresh Block needs an observed predecessor HEAD, absent from
	// fresh claims. Never synthesize a SHA to satisfy result validation.
	if errors.As(err, &stop) {
		return ErrPipelineInput
	}
	return err
}

func (p *claimPipeline) prepare(ctx context.Context, e *Execution) error {
	if p.deps.Audit == nil || p.deps.Copies == nil {
		return ErrPipelineInput
	}
	repositories := worktree.Repositories{WorktreeRoot: p.config.WorktreeRoot, Transport: p.deps.Read}
	if e.Claim.Worktree != nil && e.Claim.Execution.Mode == "continue" {
		if p.deps.Reuse == nil {
			return ErrPipelineInput
		}
		if err := e.ReuseWorktree(ctx, repositories, p.deps.Reuse, p.deps.Audit); err != nil {
			return err
		}
		// Do not fetch/Prepare after reuse: no base updates on continue.
		out, err := safegit.Output(ctx, e.WorktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return fmt.Errorf("runner: cannot resolve prepared Git area")
		}
		p.repository = filepath.Dir(strings.TrimSpace(string(out)))
		return nil
	}
	number := 0
	if e.Claim.Execution.Mode == "fresh" {
		if p.deps.FreshNumber == nil {
			return ErrPipelineInput
		}
		var err error
		number, err = p.deps.FreshNumber(ctx, e.Claim)
		if err != nil || number <= 0 {
			return ErrPipelineInput
		}
	}
	var err error
	if p.deps.delivery != nil {
		session, err := p.readSession(ctx, e)
		if err != nil {
			return err
		}
		repositories.Transport = session
	}
	p.repository, err = repositories.Prepare(ctx, e.Claim.Project.ID, e.Claim.Project.GitRemoteURL)
	if err != nil {
		return err
	}
	if err := e.RecordRemoteBase(ctx, repositories); err != nil {
		return err
	}
	if err := e.CreateWorktree(ctx, repositories, number, p.deps.Audit); err != nil {
		return err
	}
	return e.CopyWorktreeFiles(ctx, repositories, p.deps.Copies)
}

func (p *claimPipeline) pin(ctx context.Context, e *Execution) error {
	if p.deps.Policy == nil {
		return ErrPipelineInput
	}
	var err error
	p.policy, err = p.deps.Policy(ctx, e.Claim)
	if err != nil {
		return err
	}
	if p.policy.CPU <= 0 || p.policy.MemoryBytes <= 0 || !filepath.IsAbs(p.policy.Shell) || p.policy.JUnitFormat == "" {
		return ErrPipelineInput
	}
	if err := json.Unmarshal(e.Claim.Profile.Definition, &p.definition); err != nil {
		return err
	}
	project, err := projectcontext.Read(ctx, e.WorktreePath, e.BaseSHA)
	if err != nil {
		return err
	}
	files, err := harnesscontext.Read(ctx, p.policy.HarnessRepository, e.Claim.Profile.CommitSHA, p.definition.ContextFiles)
	if err != nil {
		return err
	}
	root := filepath.Join(p.config.WorktreeRoot, ".evaloom-configurations")
	worktreeRoot, err := os.OpenRoot(p.config.WorktreeRoot)
	if err != nil {
		return err
	}
	defer worktreeRoot.Close()
	if err := worktreeRoot.Mkdir(".evaloom-configurations", 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := worktreeRoot.Lstat(".evaloom-configurations")
	if err != nil || !info.IsDir() {
		return fmt.Errorf("runner: configuration root must be a dedicated directory")
	}
	head, err := safegit.Output(ctx, e.WorktreePath, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("runner: cannot resolve context HEAD")
	}
	p.environment, p.requestContext, err = p.runner.PrepareHarnessEnvironment(ctx, e.Claim.Execution.ID,
		strings.TrimSpace(string(head)), project, harnessenv.Config{Root: root,
			CommitSHA: e.Claim.Profile.CommitSHA, ConfigVariable: p.policy.ConfigVariable, Files: files})
	return err
}

func (p *claimPipeline) receive(_ context.Context, _ *Execution) error {
	// Spec 07 returns the Execution Context in the stage-4 POST response.
	if p.environment == nil || p.requestContext.RevisionIDs == nil {
		return ErrPipelineInput
	}
	return nil
}

func (p *claimPipeline) containerConfig() (sandbox.ContainerConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sandbox.ContainerConfig{}, err
	}
	allow := []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"}
	for name := range p.policy.Env {
		allow = append(allow, name)
	}
	return sandbox.ContainerConfig{RuntimePath: p.config.Container.RuntimePath, Image: p.config.Container.Image,
		ProxyImage: p.config.Container.ProxyImage, ProxyBinary: p.config.Container.ProxyBinary, RuntimeEnv: p.config.Container.RuntimeEnv,
		WorktreeRoot: p.config.WorktreeRoot, GitDirRoot: filepath.Join(p.config.WorktreeRoot, ".evaloom-repositories"),
		ConfigDirRoot: filepath.Join(p.config.WorktreeRoot, ".evaloom-configurations"),
		HostHome:      home, DataDir: p.config.DataDir, RunnerTokenFile: p.config.TokenFile,
		AllowEnv: allow, ControlTimeout: pipelineControlTimeout}, nil
}

func (p *claimPipeline) newSandbox(stdout, stderr io.Writer) (sandbox.Sandbox, error) {
	cfg, err := p.containerConfig()
	if err != nil {
		return nil, err
	}
	cfg.Stdout, cfg.Stderr = stdout, stderr
	if p.deps.Sandbox != nil {
		return p.deps.Sandbox(cfg)
	}
	return sandbox.NewContainer(cfg)
}

func (p *claimPipeline) limits(ctx context.Context) sandbox.ResourceLimits {
	deadline, _ := ctx.Deadline()
	return sandbox.ResourceLimits{CPU: p.policy.CPU, MemoryBytes: p.policy.MemoryBytes, Time: time.Until(deadline)}
}

func (p *claimPipeline) setup(ctx context.Context, e *Execution) error {
	stage := SetupStage{NewLog: func(id string) (*Log, error) { return p.runner.NewLog(id, "setup", nil, e.LogSecrets()...) },
		Prepare: func(ctx context.Context, req SetupRequest, out, errOut io.Writer) (sandbox.Sandbox, sandbox.SandboxSpec, []string, error) {
			s, err := p.newSandbox(out, errOut)
			spec := sandbox.SandboxSpec{Kind: sandbox.TestSetup, Worktree: req.WorktreePath,
				Env: p.policy.Env, Network: sandbox.NetworkNone, Limits: p.limits(ctx)}
			if len(p.policy.SetupDestinations) > 0 {
				spec.Network, spec.AllowedDestinations = sandbox.NetworkSetup, p.policy.SetupDestinations
			}
			return s, spec, []string{p.policy.Shell, "-c", req.Command}, err
		}}
	return stage.Run(ctx, e)
}

func (p *claimPipeline) test(ctx context.Context, e *Execution, baseline bool) error {
	log, err := p.runner.NewLog(e.Claim.Execution.ID, "test", nil, e.LogSecrets()...)
	if err != nil {
		return err
	}
	delivered := make(chan error, 1)
	go func() { delivered <- log.Deliver(e.control.ReportContext) }()
	out, outW := io.Pipe()
	errOut, errW := io.Pipe()
	read := make(chan error, 2)
	go func() { read <- log.Read(out); _ = out.Close() }()
	go func() { read <- log.Read(errOut); _ = errOut.Close() }()
	s, runErr := p.newSandbox(outW, errW)
	if runErr == nil {
		cfg := BaselineConfig{Sandbox: s, Shell: p.policy.Shell, JUnitFormat: p.policy.JUnitFormat, Limits: p.limits(ctx)}
		var handler StageFunc
		if baseline {
			handler, runErr = p.runner.BaselineHandler(cfg)
		} else {
			handler, runErr = p.runner.Gate2Handler(Gate2Config{cfg})
		}
		if runErr == nil {
			runErr = handler(ctx, e)
		}
	}
	_ = outW.Close()
	_ = errW.Close()
	readErr := errors.Join(<-read, <-read)
	return errors.Join(runErr, readErr, log.Finish(false), <-delivered)
}

func (p *claimPipeline) baseline(ctx context.Context, e *Execution) error {
	return p.test(ctx, e, true)
}
func (p *claimPipeline) verify(ctx context.Context, e *Execution) error { return p.test(ctx, e, false) }

func (p *claimPipeline) gate3(ctx context.Context, e *Execution) error {
	if e.Claim.Execution.ResumePoint != nil {
		if err := p.prepareResume(ctx, e); err != nil {
			return err
		}
	}
	handler, err := p.runner.Gate3Handler(Gate3Config{})
	if err != nil {
		return err
	}
	err = handler(ctx, e)
	if err == nil {
		p.gates = append(p.gates, e.gateResults...)
	}
	return err
}

func (p *claimPipeline) harness(ctx context.Context, e *Execution) error {
	if p.environment == nil {
		return ErrPipelineInput
	}
	adapters := p.deps.Adapters
	if adapters == nil {
		adapters = &adapter.Registry{}
		cfg, err := p.containerConfig()
		if err != nil {
			return err
		}
		resolve := func(env adapter.SandboxEnv) (sandbox.SandboxSpec, error) {
			return sandbox.SandboxSpec{Kind: sandbox.Harness, Worktree: env.Worktree, GitDir: filepath.Join(p.repository, ".git"),
				ConfigDir: env.ConfigDir, Env: env.Env, Network: sandbox.NetworkHarness,
				AllowedDestinations: p.policy.HarnessDestinations, Limits: p.limits(ctx)}, nil
		}
		auth := func(req adapter.HarnessRequest) (string, error) {
			path := p.config.HarnessAuthFiles[req.Profile.HarnessID]
			if path == "" {
				return "", ErrPipelineInput
			}
			return path, nil
		}
		original := func(env adapter.SandboxEnv) (*harnessenv.Environment, error) {
			if env.Worktree != e.WorktreePath || env.ConfigDir != p.environment.ConfigDir() {
				return nil, ErrPipelineInput
			}
			return p.environment, nil
		}
		permissions := func(context.Context, adapter.HarnessRequest) (json.RawMessage, error) {
			return e.Claim.Profile.ToolPermissions, nil
		}
		if err := adapters.Register("codex", codex.New(codex.Config{Launch: codex.ContainerLaunch(cfg, resolve),
			ResolveAuthentication: auth, ResolveConfig: original, ResolveToolPermissions: permissions, CancelGrace: pipelineCancelGrace})); err != nil {
			return err
		}
		if err := adapters.Register("claude_code", claudecode.New(claudecode.Config{Launch: claudecode.ContainerLaunch(cfg, resolve),
			ResolveAuthentication: auth, ResolveConfig: original, ResolveToolPermissions: permissions, CancelGrace: pipelineCancelGrace})); err != nil {
			return err
		}
	}
	harness, err := adapters.Lookup(e.Claim.Profile.Adapter)
	if err != nil {
		return err
	}
	env := p.environment.Env()
	for key, value := range p.policy.Env {
		env[key] = value
	}
	// A per-process Git configuration supplies the configured author, without
	// reading personal Git settings or modifying the pinned Profile files.
	env["GIT_CONFIG_COUNT"], env["GIT_CONFIG_KEY_0"], env["GIT_CONFIG_VALUE_0"] = "2", "user.name", p.config.CommitAuthor.Name
	env["GIT_CONFIG_KEY_1"], env["GIT_CONFIG_VALUE_1"] = "user.email", p.config.CommitAuthor.Email
	req := adapter.HarnessRequest{ContractVersion: adapter.ContractVersion, ExecutionID: e.Claim.Execution.ID,
		Task:    adapter.RequestTask{ID: e.Claim.Execution.TaskID, Title: e.Claim.Task.Title, Body: e.Claim.Task.Body, AcceptanceCriteria: e.Claim.Task.AcceptanceCriteria},
		Project: adapter.RequestProject{ID: e.Claim.Project.ID, Workdir: e.WorktreePath, Branch: e.BranchName, BaseSHA: e.BaseSHA},
		Profile: adapter.RequestProfile{HarnessID: e.Claim.Profile.HarnessID, Version: e.Claim.Profile.Version, CommitSHA: e.Claim.Profile.CommitSHA,
			ModelID: p.definition.ModelID, Definition: e.Claim.Profile.Definition}, Context: p.requestContext}
	id, err := harness.Start(ctx, req, adapter.SandboxEnv{Worktree: e.WorktreePath, ConfigDir: p.environment.ConfigDir(), Env: env})
	if err != nil {
		return err
	}
	e.harnessSessionID = id
	// Adapter.Result confirms all Sandbox writers have stopped, including on
	// cancellation. Drain Logs concurrently to permit streaming adapters.
	lines, logErr := harness.Logs(id)
	if lines == nil && logErr == nil {
		logErr = fmt.Errorf("runner: harness log stream is required")
	}
	log, err := p.runner.NewLog(e.Claim.Execution.ID, "harness", nil, e.LogSecrets()...)
	if err != nil || logErr != nil {
		_ = harness.Cancel(id)
		_, _ = harness.Result(id)
		if log != nil {
			_ = log.Finish(false)
		}
		return errors.Join(err, logErr)
	}
	delivered, read, collected := make(chan error, 1), make(chan error, 2), make(chan error, 1)
	go func() { delivered <- log.Deliver(e.control.ReportContext) }()
	out, outW := io.Pipe()
	errOut, errW := io.Pipe()
	go func() { read <- log.Read(out); _ = out.Close() }()
	go func() { read <- log.Read(errOut); _ = errOut.Close() }()
	go func() {
		var err error
		for line := range lines {
			var writer io.Writer
			switch line.Stream {
			case "stdout":
				writer = outW
			case "stderr":
				writer = errW
			default:
				err = errors.Join(err, fmt.Errorf("runner: invalid harness log stream"))
				continue
			}
			_, writeErr := io.WriteString(writer, line.Text+"\n")
			err = errors.Join(err, writeErr)
		}
		_ = outW.Close()
		_ = errW.Close()
		collected <- err
	}()
	result, resultErr := harness.Result(id)
	if resultErr == nil && result.SessionID != id {
		resultErr = fmt.Errorf("runner: harness session mismatch")
	}
	e.HarnessResult = &result
	err = errors.Join(resultErr, ctx.Err(), <-collected, <-read, <-read, log.Finish(false), <-delivered)
	if err != nil {
		return err
	}
	if err := result.Validate(); err != nil {
		return err
	}
	if result.Status == adapter.ResultFailed {
		class := model.ErrorExecutionFailed
		if result.ErrorClass != nil {
			class = *result.ErrorClass
		}
		if class == model.ErrorTimeout {
			return context.DeadlineExceeded
		}
		if class == model.ErrorCancelled {
			return context.Canceled
		}
		return errors.Join(ErrHarnessFailed, e.report(model.ExecutionFailed, class))
	}
	return nil
}

func (p *claimPipeline) publish(ctx context.Context, e *Execution) error {
	if e.Claim.Execution.ResumePoint != nil && *e.Claim.Execution.ResumePoint == "publish" {
		if err := p.prepareResume(ctx, e); err != nil {
			return err
		}
	}
	if p.deps.Publication == nil || e.HarnessResult == nil || e.VerifyRun == nil {
		return ErrPipelineInput
	}
	cfg, err := p.deps.Publication(ctx, e.Claim)
	if err != nil {
		return err
	}
	if cfg.Recorder == nil || cfg.Push.RecordRemoteMismatch == nil {
		return ErrPipelineInput
	}
	if p.deps.delivery != nil {
		p.deps.delivery.branch = e.BranchName
	}
	if cfg.PullRequests == nil || cfg.Push.Git == nil {
		g, err := p.github(e)
		if err != nil {
			return err
		}
		cfg.PullRequests, cfg.Push.Git = g, g
	}
	cfg.Push.RepositoryURL, cfg.Push.CheckCancellation = e.Claim.Project.GitRemoteURL, p.runner.Heartbeat
	cfg.BaseBranch, cfg.ReportContext = e.Claim.Project.BaseBranch, e.control.ReportContext
	cfg.Report = publisher.Report{ExecutionID: e.Claim.Execution.ID, TaskID: e.Claim.Execution.TaskID, Title: e.Claim.Task.Title,
		AcceptanceCriteria: e.Claim.Task.AcceptanceCriteria, Summary: e.HarnessResult.Summary, HarnessID: e.Claim.Profile.HarnessID,
		HarnessVersion: p.definition.HarnessVersion, ProfileVersion: e.Claim.Profile.Version, ProfileCommitSHA: e.Claim.Profile.CommitSHA,
		VerifyRun: *e.VerifyRun}
	// State reporting flushes e.gateResults before this handler; retain the
	// actual results/findings, including the count-only check annotation.
	for _, gate := range p.gates {
		switch gate.Gate {
		case 2:
			cfg.Report.Gate2Result, cfg.Report.Gate2Findings = gate.Result, gate.Findings
		case 3:
			cfg.Report.Gate3Result, cfg.Report.Gate3Findings = gate.Result, gate.Findings
		}
	}
	if block := e.Claim.Execution.ResumeBlock; block != nil {
		detail, err := json.Marshal(block)
		if err != nil {
			return err
		}
		cfg.Report.ContinuedBlocks = []string{string(detail)}
	}
	_, err = publisher.Publish(ctx, publisher.Target{Worktree: e.WorktreePath, VerifiedSHA: e.VerifiedSHA,
		BranchName: e.BranchName}, cfg)
	return err
}
