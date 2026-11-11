package contract

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?(\+([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?$`)
var slotPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
var hostnamePattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// The contract requires a digest but does not choose a digest algorithm.
var digestPattern = regexp.MustCompile(`^\S+@[A-Za-z][A-Za-z0-9_+.-]*:[A-Za-z0-9=_-]+$`)

func validSemver(value string) bool {
	if !semverPattern.MatchString(value) {
		return false
	}
	core, _, _ := strings.Cut(value, "+")
	_, pre, _ := strings.Cut(core, "-")
	for _, part := range strings.Split(pre, ".") {
		if len(part) > 1 && part[0] == '0' && strings.Trim(part, "0123456789") == "" {
			return false
		}
	}
	return true
}

func (p ProfileDefinition) Validate() error {
	if p.ContractVersion != Version {
		return fmt.Errorf("contractVersion: expected %q", Version)
	}
	if !validSemver(p.ProfileVersion) {
		return fmt.Errorf("profileVersion: expected SemVer")
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("name: must not be empty")
	}
	if err := boundedText(p.Description, 2000, "description"); err != nil {
		return err
	}
	if p.AcceptanceCriteria != "required" && p.AcceptanceCriteria != "optional" {
		return fmt.Errorf("acceptanceCriteria: expected required or optional")
	}
	if p.Params != nil && p.Params.Schema != "" && !relativePath(p.Params.Schema) {
		return fmt.Errorf("params.schema: expected relative path")
	}
	for _, s := range p.programStages() {
		if s.definition == nil {
			continue
		}
		if err := s.definition.validate(s.stage); err != nil {
			return fmt.Errorf("stages.%s: %w", s.stage, err)
		}
	}
	w := p.Stages.Work
	if err := w.MicroHarness.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(w.Model) == "" || w.TimeoutSec < 1 {
		return fmt.Errorf("stages.work: model and positive timeoutSec are required")
	}
	if w.Resume != "" && w.Resume != "required" && w.Resume != "optional" {
		return fmt.Errorf("stages.work.resume: expected required or optional")
	}
	for _, files := range [][]string{w.Instructions, w.Skills, w.Settings} {
		for _, file := range files {
			if !relativePath(file) {
				return fmt.Errorf("stages.work: expected repository-relative path")
			}
		}
	}
	if len(w.ToolPermissions) > 0 {
		if err := objectJSON(w.ToolPermissions, "stages.work.toolPermissions"); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, a := range p.Approvals {
		if !operationPattern.MatchString(a.Operation) || seen[a.Operation] {
			return fmt.Errorf("approvals.operation: expected unique name of up to 64 characters")
		}
		seen[a.Operation] = true
	}
	if p.Records != nil {
		if p.Records.Pass.Recent < 0 || p.Records.Pass.Recent > 50 {
			return fmt.Errorf("records.pass.recent: expected 0–50")
		}
		for _, file := range p.Records.Keep {
			stage, file, _ := strings.Cut(file, "/")
			if stage != "prepare" && stage != "check" && stage != "publish" || !relativePath(file) {
				return fmt.Errorf("records.keep: expected prepare/check/publish relative output path")
			}
		}
	}
	return nil
}

type namedProgramStage struct {
	stage      model.Stage
	definition *ProgramStage
}

func (p ProfileDefinition) programStages() []namedProgramStage {
	return []namedProgramStage{
		{model.StagePrepare, p.Stages.Prepare},
		{model.StageCheck, &p.Stages.Check},
		{model.StagePublish, &p.Stages.Publish},
	}
}

func (p ProgramStage) validate(stage model.Stage) error {
	if len(p.Command) == 0 || !relativePath(p.Command[0]) || p.TimeoutSec < 1 {
		return fmt.Errorf("command requires relative executable path; timeoutSec must be positive")
	}
	if stage == model.StageCheck && (p.Network != nil || p.Secrets != nil) {
		return fmt.Errorf("check cannot declare network or secrets")
	}
	if p.Network != nil {
		for _, dest := range p.Network.Allow {
			if !validDestination(dest) {
				return fmt.Errorf("network.allow: expected public hostname:port, got %q", dest)
			}
		}
	}
	seen := map[string]bool{}
	for _, secret := range p.Secrets {
		if !slotPattern.MatchString(secret.Slot) || seen[secret.Slot] {
			return fmt.Errorf("secrets.slot: invalid or duplicate slot")
		}
		seen[secret.Slot] = true
		if secret.Access != "read" && secret.Access != "write" || stage == model.StagePrepare && secret.Access != "read" {
			return fmt.Errorf("secrets.access: prepare allows read; publish allows read/write")
		}
	}
	if p.Runtime != nil {
		switch p.Runtime.Kind {
		case "container":
			if !digestPattern.MatchString(p.Runtime.Image) {
				return fmt.Errorf("runtime.image: container requires an image digest")
			}
		case "macos_vm":
			if strings.TrimSpace(p.Runtime.Image) == "" {
				return fmt.Errorf("runtime.image: Tart image is required")
			}
		default:
			return fmt.Errorf("runtime.kind: expected container or macos_vm")
		}
	}
	return nil
}

func validDestination(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || net.ParseIP(host) != nil || len(host) > 253 {
		return false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strings.Trim(port, "0123456789") != "" {
		return false
	}
	host = strings.ToLower(host)
	for _, suffix := range []string{"localhost", "local", "internal", "home", "lan"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return false
		}
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !hostnamePattern.MatchString(label) {
			return false
		}
	}
	return strings.Trim(labels[len(labels)-1], "0123456789") != ""
}

// ProfileEnvironment supplies deployment-dependent limits and capabilities.
// This package does not choose global settings, Adapter permission syntax,
// known model endpoints, or whether a Runner supports macOS VMs.
type ProfileEnvironment interface {
	StageTimeoutMaxSec(model.Stage) int
	ValidateToolPermissions(model.AdapterName, json.RawMessage) error
	ValidateRuntime(Runtime) error
	ValidateNetworkDestination(string) error
}

// ValidateForRegistration adds checks that cannot be made by JSON decoding.
// Checking committed files, symlinks and schema contents belongs to Registry.
func (p ProfileDefinition) ValidateForRegistration(tagVersion string, environment ProfileEnvironment) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.ProfileVersion != tagVersion {
		return fmt.Errorf("profileVersion: differs from tag version")
	}
	if environment == nil {
		return fmt.Errorf("profile environment is required")
	}
	if max := environment.StageTimeoutMaxSec(model.StageWork); max < 1 || p.Stages.Work.TimeoutSec > max {
		return fmt.Errorf("stages.work.timeoutSec: exceeds configured limit")
	}
	if len(p.Stages.Work.ToolPermissions) > 0 {
		if err := environment.ValidateToolPermissions(p.Stages.Work.MicroHarness, p.Stages.Work.ToolPermissions); err != nil {
			return err
		}
	}
	for _, s := range p.programStages() {
		if s.definition == nil {
			continue
		}
		if max := environment.StageTimeoutMaxSec(s.stage); max < 1 || s.definition.TimeoutSec > max {
			return fmt.Errorf("stages.%s.timeoutSec: exceeds configured limit", s.stage)
		}
		if s.definition.Runtime != nil {
			if err := environment.ValidateRuntime(*s.definition.Runtime); err != nil {
				return err
			}
		}
		if s.definition.Network != nil {
			for _, destination := range s.definition.Network.Allow {
				if err := environment.ValidateNetworkDestination(destination); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
