package securitytest

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/runner"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

//go:embed probeharness/probe_stage.py
var probeStageProgram []byte

// ProbeHarnessID is the harness ID tests register the probe special harness
// under unless they choose another one. Test secrets are stored for it.
const ProbeHarnessID = "probe"

// ProbeOperation is the approval-declared operation of the probe harness.
const ProbeOperation = "probe_publish"

// ProbeAuthorize makes the publish stage ask authorize right before its one
// operation and send that operation to Destination only on allowed: true.
// ContentID is optional; by default the program recomputes the identifier
// from the prepared workspace content. Attempts (0 means 1) repeats
// ask-then-operate with the same approval, as a harness that tries to reuse
// a consumed approval would (spec 14 3.4); every reply is written to
// out/authorizations.json and the first also to out/authorization.json.
type ProbeAuthorize struct {
	Operation   string `json:"operation"`
	Destination string `json:"destination"`
	ContentID   string `json:"contentId,omitempty"`
	Attempts    int    `json:"attempts,omitempty"`
	// Concurrent starts Attempts authorize calls together on separate channel
	// connections. Operations are still sent once per allowed reply.
	Concurrent bool `json:"concurrent,omitempty"`
}

// ProbeHarness is one test case's probe special harness (spec 14 3.0). Its
// prepare, check and publish stages run bin/probe, which attempts Plans and
// writes the observations to out/probe-<stage>.json, and returns the result
// JSON merged with Results. Profile, when set, edits the definition last
// (contract-conformance cases may deliberately make it invalid).
type ProbeHarness struct {
	Plans        map[string]json.RawMessage
	Results      map[string]map[string]any
	Secrets      map[string][]contract.Secret
	Network      map[string][]string
	Authorize    *ProbeAuthorize
	Instructions string
	Profile      func(*contract.ProfileDefinition)
}

// Definition is the committed evaloom/profile.yaml (JSON is valid YAML).
func (h ProbeHarness) Definition() contract.ProfileDefinition {
	program := func(stage string) contract.ProgramStage {
		p := contract.ProgramStage{Command: []string{"bin/probe", stage}, TimeoutSec: 60, Secrets: h.Secrets[stage]}
		if allow, ok := h.Network[stage]; ok {
			p.Network = &contract.Network{Allow: allow}
		}
		return p
	}
	prepare := program("prepare")
	definition := contract.ProfileDefinition{ContractVersion: "1", ProfileVersion: "1.0.0", Name: "probe-special-harness",
		Description: "Spec 14 probe special harness. It attempts accesses and never calls an AI.", AcceptanceCriteria: "required",
		Stages: contract.ProfileStages{Prepare: &prepare,
			Work:  contract.WorkStage{MicroHarness: "codex", Model: "probe", Instructions: []string{"prompts/work.md"}, Resume: "optional", TimeoutSec: 60},
			Check: program("check"), Publish: program("publish")},
		Approvals: []contract.ApprovalDeclaration{{Operation: ProbeOperation, Description: "Send the probe operation to the test destination"}}}
	if h.Profile != nil {
		h.Profile(&definition)
	}
	return definition
}

// WriteProbeHarness writes the repository content into an empty directory.
// The caller commits it and tags profile/v1.0.0, as for any special harness.
func WriteProbeHarness(repo string, h ProbeHarness) error {
	instructions := h.Instructions
	if instructions == "" {
		instructions = "Probe work instructions. The probe Adapter never calls an AI.\n"
	}
	profile, err := json.MarshalIndent(h.Definition(), "", "  ")
	if err != nil {
		return err
	}
	access, err := assets.ReadFile("assets/probe.py")
	if err != nil {
		return err
	}
	files := map[string][]byte{"evaloom/profile.yaml": profile, "prompts/work.md": []byte(instructions),
		"bin/probe": probeStageProgram, "bin/probe_access.py": access}
	for stage, plan := range h.Plans {
		if stage != "prepare" && stage != "check" && stage != "publish" {
			return fmt.Errorf("securitytest: plan for unknown stage %q", stage)
		}
		files["probe/"+stage+"-plan.json"] = plan
	}
	for stage, result := range h.Results {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		files["probe/"+stage+"-result.json"] = data
	}
	if h.Authorize != nil {
		data, err := json.Marshal(h.Authorize)
		if err != nil {
			return err
		}
		files["probe/publish-authorize.json"] = data
	}
	for name, data := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if name == "bin/probe" {
			mode = 0700
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			return err
		}
	}
	return nil
}

// WorkProbeReport is the worktree-relative observation file of the work stage.
const WorkProbeReport = ".evaloom-probe/work-results.json"

// NewWorkProbe returns the probe Adapter for the work stage: it runs the
// access plan with python3 inside the Work Sandbox reached through Runner's
// work wiring (Launch with a working copy of the dedicated configuration, as
// production Adapters use) and requires WorkProbeReport. It never calls an
// AI and installs no model credentials. Without Launch it fails as
// unavailable instead of running on the host.
func NewWorkProbe(w runner.WorkAdapterConfig, plan json.RawMessage) (adapter.Adapter, error) {
	if !json.Valid(plan) {
		return nil, fmt.Errorf("securitytest: invalid work plan")
	}
	source, err := assets.ReadFile("assets/probe.py")
	if err != nil {
		return nil, err
	}
	var box probe.Sandbox
	if w.Launch != nil {
		box = probe.LaunchSandbox{Launch: w.Launch}
	}
	session := func(ctx context.Context, env adapter.SandboxEnv) (context.Context, adapter.SandboxEnv, func(), error) {
		if w.ResolveConfig == nil {
			return nil, env, nil, probe.ErrUnavailable
		}
		original, err := w.ResolveConfig(env)
		if err != nil {
			return nil, env, nil, err
		}
		working, err := original.WorkingCopy(ctx)
		if err != nil {
			return nil, env, nil, err
		}
		ctx, closeConfiguration, err := sandbox.WithWorkingConfiguration(ctx, working)
		if err != nil {
			working.Close()
			return nil, env, nil, err
		}
		env.ConfigDir, env.Env = working.ConfigDir(), working.Env()
		return ctx, env, func() { _ = closeConfiguration(); working.Close() }, nil
	}
	command := []string{"/usr/bin/env", "python3", "-c", string(source), "--inline-plan", string(plan), WorkProbeReport}
	return probe.NewWithConfig(probe.Config{Sandbox: box, Command: command, Report: WorkProbeReport, Session: session}), nil
}

// ProbeAccessSource is the access-attempt script (probe_access.py) for
// Sandboxes without a committed Profile, such as the Proposal Sandbox. Run
// it as python3 -c <source> --inline-plan <plan> <result>.
func ProbeAccessSource() ([]byte, error) { return assets.ReadFile("assets/probe.py") }
