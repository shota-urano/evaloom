// Package codex runs the Codex CLI through an explicitly supplied Sandbox.
// It never executes commands on the host or reads the user's configuration.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// Launch starts one command inside the prepared Harness Sandbox. Writers carry
// raw output to Logs; Runner owns redaction. Wait must confirm ALL writers have
// stopped. Launch must honor ctx during preparation; Stop/Kill must be bounded.
type Launch func(context.Context, adapter.SandboxEnv, []string, io.Writer, io.Writer) (sandbox.Process, error)

// Config is trusted Runner wiring, not a public API or Profile schema.
type Config struct {
	Launch Launch
	// Configure resolves Harness Tool Permissions using req.Profile.HarnessID
	// and intersects them with Profile toolConfiguration. Return Codex TOML
	// key=value overrides, or an error if the policy cannot be represented.
	// A missing resolver fails closed: the request has no Tool Permissions.
	Configure func(context.Context, adapter.HarnessRequest) ([]string, error)
	// CancelGrace is supplied by Runner's cancellation policy (no Q15 default).
	CancelGrace time.Duration
}

type harness struct {
	config   Config
	mu       sync.Mutex
	sessions map[adapter.SessionID]*session
}

type session struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}
	logs   []adapter.LogLine
	stream chan adapter.LogLine
	result adapter.HarnessResult
}

func New(config Config) adapter.Adapter {
	return &harness{config: config, sessions: make(map[adapter.SessionID]*session)}
}

func (*harness) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Streaming: true, TokenUsage: true, NativeInstructionFiles: []string{"AGENTS.md"}}
}

func (h *harness) Start(ctx context.Context, req adapter.HarnessRequest, env adapter.SandboxEnv) (adapter.SessionID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := req.Validate(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(env.Worktree) || env.Worktree != req.Project.Workdir {
		return "", fmt.Errorf("codex: prepared worktree must match request")
	}
	if req.Limits.TimeoutSec > int((time.Duration(1<<63-1))/time.Second) {
		return "", fmt.Errorf("codex: timeout exceeds duration range")
	}
	// Own all asynchronous inputs, including nested Profile JSON and env maps.
	req.Profile.Definition = append(json.RawMessage(nil), req.Profile.Definition...)
	req.Context.RevisionIDs = append([]string{}, req.Context.RevisionIDs...)
	env.Env = maps.Clone(env.Env)
	ctx, cancel := context.WithCancel(ctx)
	if req.Limits.TimeoutSec > 0 {
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithTimeout(ctx, time.Duration(req.Limits.TimeoutSec)*time.Second)
		parentCancel := cancel
		cancel = func() { deadlineCancel(); parentCancel() }
	}
	id := adapter.SessionID(ulid.Make().String())
	s := &session{cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	h.mu.Lock()
	h.sessions[id] = s
	h.mu.Unlock()
	go h.run(ctx, s, id, req, env)
	return id, nil
}

func (h *harness) run(ctx context.Context, s *session, id adapter.SessionID, req adapter.HarnessRequest, env adapter.SandboxEnv) {
	started := time.Now()
	result := adapter.HarnessResult{ContractVersion: adapter.ContractVersion, SessionID: id,
		Status: adapter.ResultFailed, Commits: []string{}, LogArtifactIDs: []string{}, ArtifactIDs: []string{}}
	class := adapter.ErrorConfig
	defer func() {
		if ctx.Err() != nil {
			class = adapter.ErrorCancelled
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				class = adapter.ErrorTimeout
			}
			result.Status = adapter.ResultFailed
		}
		if result.Status == adapter.ResultFailed {
			result.ErrorClass = &class
		}
		result.Usage.WallTimeMS = time.Since(started).Milliseconds()
		s.mu.Lock()
		s.result = result
		s.mu.Unlock()
		close(s.done)
		s.cancel()
	}()
	if h.config.Launch == nil || h.config.Configure == nil || h.config.CancelGrace <= 0 ||
		!filepath.IsAbs(env.ConfigDir) || env.Env["CODEX_HOME"] != env.ConfigDir || req.Profile.ModelID == "" {
		return
	}
	var definition struct {
		Execution struct {
			Adapter string   `json:"adapter"`
			Args    []string `json:"args"`
		} `json:"executionConfiguration"`
	}
	if json.Unmarshal(req.Profile.Definition, &definition) != nil ||
		(definition.Execution.Adapter != "" && definition.Execution.Adapter != "codex") || len(definition.Execution.Args) != 0 {
		return // No unreviewed Profile flags may override isolation or the prompt.
	}
	overrides, err := h.config.Configure(ctx, req)
	if err != nil {
		return
	}
	common := []string{"codex"}
	for _, value := range overrides {
		if !strings.Contains(value, "=") || strings.ContainsRune(value, 0) {
			return
		}
		common = append(common, "-c", value)
	}
	// login status loads the dedicated config and checks saved CLI auth without
	// invoking the model. A workload failure here is configuration, not execution.
	stderr := &lineWriter{session: s, stream: "stderr"}
	// A fixed shell wrapper turns a missing CLI into workload exit 127 rather
	// than a runtime entrypoint failure. All arguments remain positional data.
	preflight := append([]string{"/bin/sh", "-c", `exec "$@"`, "evaloom-codex-preflight"}, common...)
	preflight = append(preflight, "login", "status")
	preflightOut := &lineWriter{session: s, stream: "stdout"}
	class, err = h.command(ctx, env, preflight, preflightOut, stderr, adapter.ErrorConfig)
	preflightOut.finish()
	stderr.finish()
	if err != nil {
		return
	}
	data, _ := json.Marshal(req.Task)
	prompt := req.Context.Instructions + "\n\nTask input (JSON):\n" + string(data) +
		"\n\nImplement the task in this worktree and commit the changes. Do not push or create a PR."
	command := append(append([]string{}, common...), "exec", "--json", "--color", "never", "--ephemeral",
		"--sandbox", "danger-full-access", "--model", req.Profile.ModelID, "--cd", env.Worktree, "--", prompt)
	parser := &events{}
	stdout := &lineWriter{session: s, stream: "stdout", events: parser}
	class, err = h.command(ctx, env, command, stdout, stderr, adapter.ErrorExecutionFailed)
	stdout.finish()
	stderr.finish()
	result.Summary = parser.summary
	result.Usage.InputTokens, result.Usage.OutputTokens = parser.input, parser.output
	if parser.invalid {
		result.Usage.InputTokens, result.Usage.OutputTokens = nil, nil
	}
	if err == nil && parser.completed && !parser.failed && !parser.invalid {
		result.Status = adapter.ResultSucceeded
	} else if err == nil {
		class = adapter.ErrorExecutionFailed
	}
}

func (h *harness) command(ctx context.Context, env adapter.SandboxEnv, args []string, out, errOut io.Writer, failure adapter.ErrorClass) (adapter.ErrorClass, error) {
	p, err := h.config.Launch(ctx, env, args, out, errOut)
	if err != nil {
		var config *harnesscontext.ConfigError
		if errors.As(err, &config) {
			return adapter.ErrorConfig, err
		}
		var sandboxErr *sandbox.Error
		if errors.As(err, &sandboxErr) {
			return sandboxErr.ErrorClass(), err
		}
		return adapter.ErrorSandbox, err
	}
	if p == nil {
		return adapter.ErrorSandbox, fmt.Errorf("codex: sandbox returned no process")
	}
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		timer := time.NewTimer(h.config.CancelGrace)
		defer timer.Stop()
		go func() { _ = p.Stop() }()
		select {
		case <-done:
		case <-timer.C:
			_ = p.Kill()
			<-done // Never report termination while Sandbox writers remain alive.
		}
		return failure, ctx.Err()
	}
	var sandboxErr *sandbox.Error
	if errors.As(err, &sandboxErr) {
		return sandboxErr.ErrorClass(), err
	}
	code, confirmed := p.ExitCode()
	if !confirmed {
		return adapter.ErrorSandbox, fmt.Errorf("codex: unconfirmed workload exit")
	}
	if code != 0 || err != nil {
		return failure, fmt.Errorf("codex: command failed")
	}
	return failure, nil
}

func (h *harness) get(id adapter.SessionID) (*session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok {
		return nil, fmt.Errorf("codex: unknown session")
	}
	return s, nil
}

func (h *harness) Status(id adapter.SessionID) (adapter.SessionStatus, error) {
	s, err := h.get(id)
	if err != nil {
		return "", err
	}
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.result.Status == adapter.ResultSucceeded {
			return adapter.SessionSucceeded, nil
		}
		return adapter.SessionFailed, nil
	default:
		return adapter.SessionRunning, nil
	}
}

func (h *harness) Cancel(id adapter.SessionID) error {
	s, err := h.get(id)
	if err != nil {
		return err
	}
	select {
	case <-s.done:
	default:
		s.cancel()
	}
	return nil
}

func (h *harness) Result(id adapter.SessionID) (adapter.HarnessResult, error) {
	s, err := h.get(id)
	if err != nil {
		return adapter.HarnessResult{}, err
	}
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.result
	r.Commits, r.LogArtifactIDs, r.ArtifactIDs = []string{}, []string{}, []string{}
	if r.ErrorClass != nil {
		v := *r.ErrorClass
		r.ErrorClass = &v
	}
	if r.Usage.InputTokens != nil {
		v := *r.Usage.InputTokens
		r.Usage.InputTokens = &v
	}
	if r.Usage.OutputTokens != nil {
		v := *r.Usage.OutputTokens
		r.Usage.OutputTokens = &v
	}
	return r, nil
}

func (h *harness) Logs(id adapter.SessionID) (<-chan adapter.LogLine, error) {
	s, err := h.get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream == nil {
		s.stream = make(chan adapter.LogLine)
		go s.deliver()
	}
	return s.stream, nil // One shared consumer, as in Runner's log collector.
}

func (s *session) deliver() {
	defer close(s.stream)
	for {
		s.mu.Lock()
		batch := s.logs
		s.logs = nil
		s.mu.Unlock()
		for _, line := range batch {
			s.stream <- line
		}
		select {
		case <-s.done:
			s.mu.Lock()
			batch = s.logs
			s.logs = nil
			s.mu.Unlock()
			for _, line := range batch {
				s.stream <- line
			}
			return
		case <-s.wake:
		}
	}
}
