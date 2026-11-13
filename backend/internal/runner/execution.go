package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/registry"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// ExecuteClaim is the command's production execution path. A registry supplied
// by tests replaces only the micro-harness, never reporting or stage execution.
func (r *Runner) ExecuteClaim(ctx context.Context, config Config, claim Claim, factory func(WorkAdapterConfig) *adapter.Registry) error {
	e := &executionExecutor{runner: r, config: config, claim: claim, paths: DefaultProposalPaths}
	if factory == nil {
		return fmt.Errorf("runner: work Adapter factory is required")
	}
	adapters := factory(e.workAdapterConfig())
	if adapters == nil {
		return fmt.Errorf("runner: work Adapter registry is required")
	}
	e.adapters = adapters
	services := StageServices{Reporter: r, Executor: e, Adapters: adapters, Paths: e.paths}
	err := r.ExecuteStages(ctx, claim, services)
	var profileError *ProfileConfigError
	if errors.As(err, &profileError) {
		// Preflight still uses the stage-report protocol; the old transition-shaped
		// ReportResult is not accepted by the current Control Plane.
		stage := model.StageWork
		def, _ := contract.DecodeProfile(claim.Profile.Definition)
		if def.Stages.Prepare != nil {
			stage = model.StagePrepare
		}
		var resume stageResume
		if json.Unmarshal(claim.Resume, &resume) == nil && resume.Stage != "" {
			stage = resume.Stage
		}
		request, requestErr := stageRequest(claim, stage, e.paths, resume)
		if requestErr != nil {
			return requestErr
		}
		reportCtx := context.WithoutCancel(ctx)
		attempt, reportErr := r.StartStage(reportCtx, claim.Execution.ID, stage, request)
		if reportErr != nil {
			return reportErr
		}
		_, reportErr = r.FinishStage(reportCtx, claim.Execution.ID, stage, attempt, StageOutcome{RunnerError: strPtr("config_error")})
		return reportErr
	}
	return err
}

// One executor belongs to one claim; its work configuration is never shared
// with another concurrently running Execution.
type executionExecutor struct {
	runner         *Runner
	config         Config
	claim          Claim
	paths          contract.RequestPaths
	adapters       *adapter.Registry
	environment    *harnessenv.Environment
	workInvocation StageInvocation
	workSpec       sandbox.SandboxSpec
	container      sandbox.ContainerConfig
}

// WorkAdapterConfig connects registered micro-harnesses to one execution's
// sandbox. Concrete Adapter registration belongs in the command, keeping
// Runner independent of Adapter packages (and supporting additional Adapters).
type WorkAdapterConfig struct {
	Launch                 func(context.Context, adapter.SandboxEnv, []string, io.Writer, io.Writer) (sandbox.Process, error)
	ResolveConfig          func(adapter.SandboxEnv) (*harnessenv.Environment, error)
	ResolveAuthentication  func(adapter.HarnessRequest) (string, error)
	ResolveToolPermissions func(context.Context, adapter.HarnessRequest) (json.RawMessage, error)
	CancelGrace            time.Duration
}

func (e *executionExecutor) workAdapterConfig() WorkAdapterConfig {
	resolveConfig := func(env adapter.SandboxEnv) (*harnessenv.Environment, error) {
		if e.environment == nil || env.ConfigDir != e.environment.ConfigDir() {
			return nil, fmt.Errorf("runner: unknown work configuration")
		}
		return e.environment, nil
	}
	resolveAuth := func(adapter.HarnessRequest) (string, error) {
		name := e.workInvocation.Profile.Definition.Stages.Work.MicroHarness
		path, ok := e.config.MicroHarnessAuthFiles[name]
		if !ok {
			return "", fmt.Errorf("runner: missing micro-harness authentication")
		}
		return path, nil
	}
	permissions := func(context.Context, adapter.HarnessRequest) (json.RawMessage, error) {
		raw := e.workInvocation.Profile.Definition.Stages.Work.ToolPermissions
		if len(raw) == 0 {
			raw = json.RawMessage(`{}`)
		}
		return raw, nil
	}
	launch := func(ctx context.Context, env adapter.SandboxEnv, command []string, stdout, stderr io.Writer) (sandbox.Process, error) {
		spec := e.workSpec
		spec.ConfigDir, spec.ConfigWritable = env.ConfigDir, true
		spec.Env = maps.Clone(e.workSpec.Env)
		maps.Copy(spec.Env, env.Env)
		spec.Env["EVALOOM_REQUEST"] = "/evaloom/request.json"
		config := e.container
		config.Stdout, config.Stderr = stdout, stderr
		box, err := sandbox.NewContainer(config)
		if err != nil {
			return nil, err
		}
		return box.RunControlled(ctx, spec, command)
	}
	return WorkAdapterConfig{Launch: launch, ResolveConfig: resolveConfig, ResolveAuthentication: resolveAuth, ResolveToolPermissions: permissions, CancelGrace: StageCancelGrace}
}

func (e *executionExecutor) RunStage(ctx context.Context, in StageInvocation) StageOutcome {
	reportCtx := context.WithoutCancel(ctx)
	// Scratch is disposable even when result collection fails; out is retained.
	defer os.RemoveAll(filepath.Join(in.Directory, "scratch"))
	spec, config, err := e.stageSandbox(in)
	if err != nil {
		return StageOutcome{RunnerError: strPtr("config_error")}
	}
	if in.Stage == model.StageWork {
		if err := e.readWorkCredentials(in); err != nil {
			return StageOutcome{RunnerError: strPtr("config_error")}
		}
	}
	log, err := in.NewLog(nil)
	if err != nil {
		return StageOutcome{RunnerError: strPtr("config_error")}
	}
	delivered := make(chan error, 1)
	go func() { delivered <- log.Deliver(reportCtx) }()
	var outcome StageOutcome
	if in.Stage == model.StageWork {
		outcome = e.runWork(ctx, in, spec, config, log)
		// The Adapter has released its credential lock after writeback. Include
		// refreshed values before collecting work files or retaining inputs.
		if err := e.readWorkCredentials(in); err != nil && outcome.RunnerError == nil {
			outcome.RunnerError = strPtr("config_error")
		}
	} else {
		// Pipes let Log.Read collect lines while the real container supervisor owns
		// the runtime and waits for all attach writers to stop.
		reader, writer := io.Pipe()
		readDone := make(chan error, 1)
		go func() { readDone <- log.Read(reader); reader.Close() }()
		config.Stdout, config.Stderr = writer, writer
		box, createErr := sandbox.NewContainer(config)
		if createErr != nil {
			outcome.RunnerError = strPtr("config_error")
		} else {
			outcome = in.RunProgram(ctx, box.RunControlled, spec)
		}
		writer.Close()
		if err := <-readDone; err != nil && outcome.RunnerError == nil {
			outcome.RunnerError = strPtr("sandbox_error")
		}
		raw, readErr := readStageFile(in.Directory, "scratch/result.json")
		if outcome.RunnerError != nil && readErr == nil && json.Valid(raw) {
			candidate := outcome
			candidate.Result = raw
			if cooperativeCancellation(ctx, in.Stage, candidate) {
				outcome.RunnerError = nil
			}
		}
		if outcome.RunnerError == nil {
			// Result is writable only inside scratch; the retained copy is outside
			// every mount and is subsequently redacted by the stage security layer.
			if readErr != nil || !json.Valid(raw) {
				outcome.RunnerError = strPtr("contract_error")
			} else {
				outcome.Result = in.redactBytes(raw)
				if err := os.WriteFile(filepath.Join(in.Directory, "result.json"), outcome.Result, 0600); err != nil {
					outcome.RunnerError = strPtr("contract_error")
				} else {
					artifact, err := e.collect(reportCtx, in, "result_json", "result.json", outcome.Result)
					if err != nil {
						outcome.RunnerError = strPtr("contract_error")
					} else {
						outcome.ResultArtifactID = &artifact.ID
					}
				}
			}
		}
	}
	if err := log.Finish(false); err != nil && outcome.RunnerError == nil {
		outcome.RunnerError = strPtr("sandbox_error")
	}
	if err := <-delivered; err != nil && outcome.RunnerError == nil {
		outcome.RunnerError = strPtr("sandbox_error")
	}
	if err := e.collectOut(reportCtx, in, &outcome); err != nil && outcome.RunnerError == nil {
		outcome.RunnerError = strPtr("contract_error")
	}
	if outcome.RunnerError != nil {
		outcome.Result = nil
		outcome.ResultArtifactID = nil
		if outcome.Work != nil {
			outcome.Work.Question = nil
		}
	}
	return outcome
}

func (e *executionExecutor) readWorkCredentials(in StageInvocation) error {
	path := e.config.MicroHarnessAuthFiles[in.Profile.Definition.Stages.Work.MicroHarness]
	if path == "" {
		return fmt.Errorf("runner: missing micro-harness authentication")
	}
	values, err := harnessenv.ReadAuthenticationValues(path)
	if err != nil {
		return err
	}
	in.security.credentials = append(in.security.credentials, values...)
	return nil
}

func (e *executionExecutor) stageSandbox(in StageInvocation) (sandbox.SandboxSpec, sandbox.ContainerConfig, error) {
	p := e.paths
	records := filepath.Join(in.Directory, "records")
	if err := os.Mkdir(records, 0700); err != nil {
		return sandbox.SandboxSpec{}, sandbox.ContainerConfig{}, err
	}
	roots := map[string]string{p.Profile: in.Profile.Path, p.Workspace: in.Workspace, p.Out: filepath.Join(in.Directory, "out"), p.Inputs: filepath.Join(in.Directory, "inputs"), p.Records: records, p.Scratch: filepath.Join(in.Directory, "scratch")}
	spec := sandbox.SandboxSpec{Kind: sandbox.SandboxKind(in.Stage), Limits: e.config.Container.ResourceLimits(in.timeout), Network: sandbox.NetworkNone,
		Env: map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8", "EVALOOM_REQUEST": "/evaloom/request.json"}}
	permissions := map[string]bool{p.Out: true, p.Scratch: true}
	if in.Stage == model.StageWork {
		permissions[p.Workspace] = true
		permissions[p.Inputs] = false
		permissions[p.Records] = false
	} else {
		permissions[p.Profile] = false
		permissions[p.Workspace] = in.Stage == model.StagePrepare
		if in.Stage != model.StagePrepare {
			permissions[p.Inputs] = false
		}
		spec.ChannelPath = in.ChannelPath
		spec.Env["EVALOOM_CHANNEL"] = "/evaloom/channel.sock"
		spec.Env["EVALOOM_RESULT"] = p.Scratch + "/result.json"
	}
	for guest, writable := range permissions {
		spec.Mounts = append(spec.Mounts, sandbox.Mount{HostPath: roots[guest], GuestPath: guest, Writable: writable})
	}
	var program *contract.ProgramStage
	switch in.Stage {
	case model.StagePrepare:
		program = in.Profile.Definition.Stages.Prepare
	case model.StageCheck:
		program = &in.Profile.Definition.Stages.Check
	case model.StagePublish:
		program = &in.Profile.Definition.Stages.Publish
	}
	image := e.config.Container.Image
	if program != nil {
		if program.Runtime != nil {
			if program.Runtime.Kind != "container" {
				return spec, sandbox.ContainerConfig{}, fmt.Errorf("runner: unsupported runtime")
			}
			image = program.Runtime.Image
		}
		if program.Network != nil {
			spec.Network = sandbox.NetworkDeclared
			spec.AllowedDestinations = program.Network.Allow
		}
	}
	modelAPIs := map[string]bool{}
	for _, name := range e.adapters.Names() {
		a, _ := e.adapters.Lookup(name)
		if d, ok := a.(registry.ModelAPIDeclarer); ok {
			for _, host := range d.ModelAPIDestinations() {
				modelAPIs[host] = true
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return spec, sandbox.ContainerConfig{}, err
	}
	config := sandbox.ContainerConfig{RuntimePath: e.config.Container.RuntimePath, Image: image, ProxyImage: e.config.Container.ProxyImage, ProxyBinary: e.config.Container.ProxyBinary, RuntimeEnv: e.config.Container.RuntimeEnv,
		StagePaths: p, StageMountRoots: roots, HostHome: home, DataDir: e.config.DataDir, RunnerTokenFile: e.config.TokenFile, ControlTimeout: StageCancelGrace,
		StageRequestFile: filepath.Join(in.Directory, "request.json"), StageRequestPath: "/evaloom/request.json", NetworkEnvironment: registry.Environment{ModelAPIs: modelAPIs}}
	return spec, config, nil
}

func readStageFile(directory, name string) ([]byte, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("runner: unsafe output file")
	}
	// os.Root confines paths; non-regular outputs never become API artifacts.
	return root.ReadFile(name)
}

func (e *executionExecutor) collect(ctx context.Context, in StageInvocation, kind, name string, data []byte) (LogArtifact, error) {
	artifact, err := in.CollectArtifact(kind, name, bytes.NewReader(data))
	if err == nil {
		err = e.runner.deliverArtifact(ctx, artifact)
	}
	return artifact, err
}

func (e *executionExecutor) collectOut(ctx context.Context, in StageInvocation, outcome *StageOutcome) error {
	if in.Stage == model.StageWork {
		if err := e.collectWorkOutputs(ctx, in, outcome); err != nil {
			return err
		}
	}
	return filepath.WalkDir(filepath.Join(in.Directory, "out"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(filepath.Join(in.Directory, "out"), path)
		if in.Stage == model.StageWork && (name == "summary.md" || name == "question.json") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("runner: unsafe output")
		}
		data, err := readStageFile(in.Directory, filepath.Join("out", name))
		if err != nil {
			return err
		}
		_, err = e.collect(ctx, in, "out_file", filepath.ToSlash(name), data)
		if err != nil {
			return err
		}
		return nil
	})
}

func (e *executionExecutor) runWork(ctx context.Context, in StageInvocation, spec sandbox.SandboxSpec, config sandbox.ContainerConfig, log *Log) StageOutcome {
	var id adapter.SessionID
	fail := func(code string) StageOutcome {
		out := StageOutcome{RunnerError: &code}
		if id != "" {
			session := string(id)
			out.Work = &StageWork{SessionID: &session}
		}
		return out
	}
	work := in.Profile.Definition.Stages.Work
	auth, ok := e.config.MicroHarnessAuthFiles[work.MicroHarness]
	if !ok || auth == "" {
		return fail("config_error")
	}
	var paths []string
	paths = append(paths, work.Instructions...)
	paths = append(paths, work.Skills...)
	paths = append(paths, work.Settings...)
	files, err := harnesscontext.Read(ctx, e.claim.Profile.RepoPath, e.claim.Profile.CommitSHA, paths)
	if err != nil {
		return fail("config_error")
	}
	root := filepath.Join(in.Directory, "configuration")
	if os.Mkdir(root, 0700) != nil {
		return fail("config_error")
	}
	variable := "CODEX_HOME"
	if work.MicroHarness == "claude_code" {
		variable = "CLAUDE_CONFIG_DIR"
	}
	environment, err := harnessenv.Create(ctx, harnessenv.Config{Root: root, CommitSHA: e.claim.Profile.CommitSHA,
		ConfigVariable: variable, Files: files, Instructions: work.Instructions})
	if err != nil {
		return fail("config_error")
	}
	defer environment.Close()
	if _, err := environment.Files(ctx); err != nil {
		return fail("config_error")
	}
	e.environment, e.workInvocation = environment, in
	config.ConfigDirRoot = root
	if declarer, ok := in.Adapter.(registry.ModelAPIDeclarer); ok {
		config.ModelAPI = declarer
		spec.Network = sandbox.NetworkHarness
	}
	e.container, e.workSpec = config, spec
	// Context revisions describe the original Profile files. The CLI's Q142
	// instruction file may replace a source path with concatenated contents.
	executionContext, err := e.pinWorkContext(ctx, in, files)
	if err != nil {
		return fail("config_error")
	}
	request, err := readStageFile(in.Directory, "request.json")
	if err != nil {
		return fail("config_error")
	}
	// The request contains the current answer; context snapshots themselves stay
	// fixed across question resumes. Task text remains delimited JSON data.
	instructions := executionContext.Instructions + "\n\nStage request (JSON data):\n" + string(request)
	req := adapter.HarnessRequest{ContractVersion: adapter.ContractVersion, ExecutionID: e.claim.Execution.ID,
		Task:    adapter.RequestTask{ID: e.claim.Task.ID, Title: e.claim.Task.Title, Body: e.claim.Task.Body, AcceptanceCriteria: e.claim.Task.AcceptanceCriteria},
		Project: adapter.RequestProject{Workdir: e.paths.Workspace},
		Profile: adapter.RequestProfile{HarnessID: e.claim.Profile.HarnessID, Version: e.claim.Profile.Version, CommitSHA: e.claim.Profile.CommitSHA, ModelID: work.Model, Definition: e.claim.Profile.Definition},
		Context: adapter.RequestContext{Instructions: instructions, RevisionIDs: executionContext.RevisionIDs}, Limits: adapter.RequestLimits{TimeoutSec: int(in.timeout / time.Second)}}
	env := adapter.SandboxEnv{Worktree: e.paths.Workspace, ConfigDir: environment.ConfigDir(), Env: environment.Env()}
	if in.WorkSessionID != nil && in.Adapter.Capabilities().Resume {
		resumable, ok := in.Adapter.(adapter.ResumableAdapter)
		if !ok {
			return fail("work_failed")
		}
		id = adapter.SessionID(*in.WorkSessionID)
		err = resumable.Resume(ctx, id, req, env)
	} else if in.WorkSessionID != nil && work.Resume == "required" {
		return fail("work_failed")
	} else {
		id, err = in.Adapter.Start(ctx, req, env)
	}
	if err != nil {
		if ctx.Err() != nil {
			return fail(stageInterruption(ctx.Err()))
		}
		return fail("work_failed")
	}
	lines, err := in.Adapter.Logs(id)
	if err != nil {
		_ = in.Adapter.Cancel(id)
		_, _ = in.WaitWork(ctx, in.Adapter, id)
		return fail("work_failed")
	}
	logsDone := make(chan error, 1)
	go func() {
		var err error
		for line := range lines {
			if readErr := log.Read(strings.NewReader(line.Text + "\n")); readErr != nil {
				err = readErr
			}
		}
		logsDone <- err
	}()
	result, err := in.WaitWork(ctx, in.Adapter, id)
	logErr := <-logsDone
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return fail(stageInterruption(err))
		}
		return fail("work_failed")
	}
	if logErr != nil {
		return fail("work_failed")
	}
	if result.Status != adapter.ResultSucceeded {
		code := "work_failed"
		if result.ErrorClass != nil && (*result.ErrorClass == model.ErrorConfig || *result.ErrorClass == model.ErrorSandbox) {
			code = string(*result.ErrorClass)
		}
		return fail(code)
	}
	exit := 0
	session := string(result.SessionID)
	return StageOutcome{ExitCode: &exit, Work: &StageWork{SessionID: &session}}
}
