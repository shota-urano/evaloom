// Package probe extends the dummy adapter's deterministic contract with script
// execution. Only tests register it. It supplies no sandbox implementation and
// never falls back to executing commands on the host.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

// Sandbox is the test-tool boundary pending spec 09's concrete implementation.
// Check must verify availability, not merely the presence of a configuration.
// Run must stop all its processes before returning when ctx is cancelled.
// ReadFile resolves a worktree-relative path in the same sandbox environment.
type Sandbox interface {
	Check(context.Context) error
	Run(context.Context, adapter.SandboxEnv, []string) (Output, error)
	ReadFile(context.Context, adapter.SandboxEnv, string) ([]byte, error)
}

// EnvChecker is a Sandbox whose availability can only be proven with the
// session's environment. Start then calls CheckEnv instead of Check.
type EnvChecker interface {
	CheckEnv(context.Context, adapter.SandboxEnv) error
}

type Output struct {
	ExitCode       int
	Stdout, Stderr string
}

var ErrUnavailable = errors.New("probe: 実行不能 (Sandbox の実体が必要)")

type harness struct {
	sandbox  Sandbox
	command  []string
	report   string
	prepare  func(context.Context, adapter.SandboxEnv) (context.Context, adapter.SandboxEnv, func(), error)
	mu       sync.Mutex
	sessions map[adapter.SessionID]*session
}

type session struct {
	cancel context.CancelFunc
	done   chan struct{}
	result adapter.HarnessResult
	logs   []adapter.LogLine
}

// New runs the legacy fixture script (python3 harness.py) and reads
// harness-results.json from the worktree.
func New(sandbox Sandbox) adapter.Adapter {
	return NewWithConfig(Config{Sandbox: sandbox, Command: []string{"python3", "harness.py"}, Report: "harness-results.json"})
}

// Config selects the deterministic script the work stage runs. Command runs
// in the Sandbox with the worktree as its working directory; Report is the
// worktree-relative observation file it must write (DecodeReport format).
//
// Session, when set, prepares the session before the Sandbox check (for
// example the Work Sandbox's working configuration copy, as production
// Adapters do). It returns the context and environment the session uses and
// a release function called once the session has stopped.
type Config struct {
	Sandbox Sandbox
	Command []string
	Report  string
	Session func(context.Context, adapter.SandboxEnv) (context.Context, adapter.SandboxEnv, func(), error)
}

func NewWithConfig(config Config) adapter.Adapter {
	return &harness{sandbox: config.Sandbox, command: append([]string(nil), config.Command...), report: config.Report,
		prepare: config.Session, sessions: make(map[adapter.SessionID]*session)}
}

func (h *harness) Start(ctx context.Context, req adapter.HarnessRequest, env adapter.SandboxEnv) (adapter.SessionID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := req.Validate(); err != nil {
		return "", err
	}
	if env.Worktree == "" || env.Worktree != req.Project.Workdir {
		return "", fmt.Errorf("probe: worktree must match request")
	}
	if len(h.command) == 0 || h.report == "" {
		return "", fmt.Errorf("probe: command and report are required")
	}
	if h.sandbox == nil {
		return "", ErrUnavailable
	}
	ctx, cancel := context.WithCancel(ctx)
	release := func() {}
	if h.prepare != nil {
		var err error
		ctx, env, release, err = h.prepare(ctx, env)
		if err != nil {
			cancel()
			return "", errors.Join(ErrUnavailable, err)
		}
	}
	var check error
	if c, ok := h.sandbox.(EnvChecker); ok {
		check = c.CheckEnv(ctx, env)
	} else {
		check = h.sandbox.Check(ctx)
	}
	if check != nil {
		release()
		cancel()
		return "", errors.Join(ErrUnavailable, check)
	}
	// Own the environment snapshot for this asynchronous session.
	vars := make(map[string]string, len(env.Env))
	for key, value := range env.Env {
		vars[key] = value
	}
	env.Env = vars
	id := adapter.SessionID(ulid.Make().String())
	s := &session{cancel: cancel, done: make(chan struct{})}
	h.mu.Lock()
	h.sessions[id] = s
	h.mu.Unlock()
	go func() {
		defer close(s.done)
		defer release()
		defer cancel()
		s.result = adapter.HarnessResult{
			ContractVersion: adapter.ContractVersion, SessionID: id,
			Status: adapter.ResultSucceeded, Summary: "probe completed " + req.ExecutionID,
			Commits: []string{}, LogArtifactIDs: []string{}, ArtifactIDs: []string{},
		}
		out, err := h.sandbox.Run(ctx, env, h.command)
		for _, line := range []adapter.LogLine{{Stream: "stdout", Text: out.Stdout}, {Stream: "stderr", Text: out.Stderr}} {
			if line.Text != "" {
				s.logs = append(s.logs, line)
			}
		}
		class := adapter.ErrorSandbox
		if err == nil {
			class = adapter.ErrorExecutionFailed
			if out.ExitCode != 0 {
				err = fmt.Errorf("probe: script failed")
			} else {
				var data []byte
				data, err = h.sandbox.ReadFile(ctx, env, h.report)
				if err == nil {
					_, err = DecodeReport(data)
				}
			}
		}
		if ctx.Err() != nil {
			err, class = ctx.Err(), adapter.ErrorCancelled
			if errors.Is(err, context.DeadlineExceeded) {
				class = adapter.ErrorTimeout
			}
		}
		if err != nil {
			s.result.Status, s.result.ErrorClass = adapter.ResultFailed, &class
			s.result.Summary = "probe failed (script or result unavailable)"
		}
	}()
	return id, nil
}

func (h *harness) get(id adapter.SessionID) (*session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok {
		return nil, fmt.Errorf("probe: unknown session")
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
		if s.result.Status == adapter.ResultSucceeded {
			return adapter.SessionSucceeded, nil
		}
		return adapter.SessionFailed, nil
	default:
		return adapter.SessionRunning, nil
	}
}

func (h *harness) Result(id adapter.SessionID) (adapter.HarnessResult, error) {
	s, err := h.get(id)
	if err != nil {
		return adapter.HarnessResult{}, err
	}
	select {
	case <-s.done:
		r := s.result
		r.Commits = append([]string{}, r.Commits...)
		r.LogArtifactIDs = append([]string{}, r.LogArtifactIDs...)
		r.ArtifactIDs = append([]string{}, r.ArtifactIDs...)
		if r.ErrorClass != nil {
			class := *r.ErrorClass
			r.ErrorClass = &class
		}
		return r, nil
	default:
		return adapter.HarnessResult{}, fmt.Errorf("probe: session is still running")
	}
}

func (h *harness) Logs(id adapter.SessionID) (<-chan adapter.LogLine, error) {
	s, err := h.get(id)
	if err != nil {
		return nil, err
	}
	// This adapter declares Streaming=false; logs become available at completion.
	<-s.done
	lines := make(chan adapter.LogLine, len(s.logs))
	for _, line := range s.logs {
		lines <- line
	}
	close(lines)
	return lines, nil
}

func (h *harness) Cancel(id adapter.SessionID) error {
	s, err := h.get(id)
	if err == nil {
		s.cancel()
	}
	return err
}

func (*harness) Capabilities() adapter.Capabilities { return adapter.Capabilities{} }

// Observation is evidence of an attempted access, not a security verdict.
// Files are controlled by the harness; callers must also inspect trusted logs
// and remote refs. A successful denied-access probe means the boundary failed.
type Observation struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Succeeded *bool  `json:"succeeded"`
	Detail    any    `json:"detail"`
}

func DecodeReport(data []byte) ([]Observation, error) {
	var report struct {
		Observations []Observation `json:"observations"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("probe: trailing report data")
	}
	if len(report.Observations) == 0 {
		return nil, fmt.Errorf("probe: no observations")
	}
	seen := make(map[string]bool)
	for _, observation := range report.Observations {
		if observation.ID == "" || observation.Kind == "" || observation.Succeeded == nil || seen[observation.ID] {
			return nil, fmt.Errorf("probe: incomplete or duplicate observation")
		}
		seen[observation.ID] = true
	}
	return report.Observations, nil
}
