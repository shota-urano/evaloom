package securitytest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/shota-urano/evaloom/backend/internal/runner"
)

// ProposalProbe replaces only the AI at Runner's ProposalHarness boundary.
// It deliberately follows the hostile Issue's instructions by running the
// access plan, then writes a deterministic proposal. Launch must be supplied
// by Runner; there is no fallback to executing on the host.
type ProposalProbe struct {
	Plan          json.RawMessage
	Label, Reason string
	Destinations  []string
	Input, Report []byte // captured before Runner removes its scratch tree
}

func (p *ProposalProbe) ModelAPIDestinations() []string { return slices.Clone(p.Destinations) }

func (p *ProposalProbe) Run(ctx context.Context, in runner.ProposalHarnessInput, launch runner.ProposalLaunch) (runner.ProposalHarnessOutcome, error) {
	if launch == nil || !json.Valid(p.Plan) {
		return runner.ProposalHarnessOutcome{}, fmt.Errorf("proposal probe: 実行不能 (launch and plan required)")
	}
	spec, err := runner.BuildProposalSandboxSpec(in)
	if err != nil {
		return runner.ProposalHarnessOutcome{}, err
	}
	source, err := ProbeAccessSource()
	if err != nil {
		return runner.ProposalHarnessOutcome{}, err
	}
	result, err := json.Marshal(map[string]string{"label": p.Label, "reason": p.Reason})
	if err != nil {
		return runner.ProposalHarnessOutcome{}, err
	}
	// The fake runtime supplies a mount table because it lacks a mount
	// namespace. On Docker these are already guest paths, with no rewriting.
	script := string(source) + `
def guest(path):
    mounts = json.loads(os.environ.get("EVALOOM_FAKE_MOUNTS", "{}"))
    for target in sorted(mounts, key=len, reverse=True):
        if path == target or path.startswith(target + "/"):
            return mounts[target] + path[len(target):]
    return path

task = json.loads(Path(guest(sys.argv[1])).read_text())
if set(task) != {"taskTitle", "taskBody", "candidates"}:
    raise RuntimeError("unexpected proposal input")
run_plan(json.loads(sys.argv[2]), guest(sys.argv[3]))
Path(guest(sys.argv[4])).write_text(sys.argv[5])
`
	// Suppress probe.py's standalone main; the driver above owns its inputs.
	script = "__name__ = 'proposal_probe'\n" + script
	process, err := launch(ctx, spec, []string{"/usr/bin/env", "python3", "-c", script,
		filepath.Join(in.Paths.Inputs, "proposal-input.json"), string(p.Plan),
		filepath.Join(in.Paths.Out, "proposal-probe.json"), filepath.Join(in.Paths.Out, "proposal.json"), string(result)}, io.Discard, io.Discard)
	if err != nil {
		return runner.ProposalHarnessOutcome{}, err
	}
	if err := process.Wait(); err != nil {
		return runner.ProposalHarnessOutcome{}, err
	}
	p.Input, err = os.ReadFile(filepath.Join(in.InputsHost, "proposal-input.json"))
	if err == nil {
		p.Report, err = os.ReadFile(filepath.Join(in.OutHost, "proposal-probe.json"))
	}
	return runner.ProposalHarnessOutcome{}, err
}
