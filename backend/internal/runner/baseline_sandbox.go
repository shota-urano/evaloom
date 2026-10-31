package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/safegit"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// SandboxBaseline connects acquisition to the existing isolated runtime and
// process supervisor. Prepare supplies trusted mounts, resource limits and
// command argv without selecting defaults absent from the specs. It must also
// arrange a fresh report; OpenReport returns nil, nil when none was produced.
// Callers must stop all other writers to this worktree for the entire call.
type SandboxBaseline struct {
	Sandbox    sandbox.Sandbox
	Prepare    func(context.Context, BaselineTestRequest) (sandbox.SandboxSpec, []string, error)
	OpenReport func(context.Context, BaselineTestRequest) (io.ReadCloser, error)
}

var _ BaselineTestSandbox = SandboxBaseline{}

func (s SandboxBaseline) RunBaseline(ctx context.Context, execution *Execution, request BaselineTestRequest) (BaselineTestOutput, error) {
	if s.Sandbox == nil || s.Prepare == nil || s.OpenReport == nil || execution == nil || execution.control == nil ||
		request.Kind != sandbox.TestRun || request.WorktreePath == "" || request.TargetSHA == "" {
		return BaselineTestOutput{}, fmt.Errorf("runner: baseline sandbox configuration and execution control are required")
	}
	spec, command, err := s.Prepare(ctx, request)
	if err != nil {
		return BaselineTestOutput{}, err
	}
	if spec.Kind != sandbox.TestRun || spec.Worktree != request.WorktreePath || len(command) == 0 ||
		spec.Network != "" && spec.Network != sandbox.NetworkNone {
		return BaselineTestOutput{}, fmt.Errorf("runner: invalid baseline test sandbox specification")
	}
	// Check after preparation and before starting the workload. Never checkout
	// or reset a continued worktree to manufacture a baseline at another SHA.
	head, err := safegit.Output(ctx, spec.Worktree, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		if ctx.Err() != nil {
			return BaselineTestOutput{}, ctx.Err()
		}
		return BaselineTestOutput{}, fmt.Errorf("runner: cannot read baseline start commit")
	}
	if strings.TrimSpace(string(head)) != request.TargetSHA {
		return BaselineTestOutput{}, fmt.Errorf("runner: baseline HEAD differs from start commit")
	}
	process, err := s.Sandbox.Run(ctx, spec, command)
	if err != nil {
		return BaselineTestOutput{}, err
	}
	if process == nil {
		return BaselineTestOutput{}, fmt.Errorf("runner: baseline sandbox returned no process")
	}
	err = execution.WaitProcess(ctx, process)
	var exit *sandbox.ExitError
	if err != nil && !errors.As(err, &exit) {
		return BaselineTestOutput{}, err
	}
	output := BaselineTestOutput{TargetSHA: request.TargetSHA}
	if code, known := process.ExitCode(); known {
		output.ExitCode = &code
	}
	output.Report, err = s.OpenReport(ctx, request)
	return output, err
}
