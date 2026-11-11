package contract

import (
	"encoding/json"
	"fmt"
	"net"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?(\+([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?$`)
var slotPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
var hostnameLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// The contract requires a digest but does not choose a digest algorithm.
var digestPattern = regexp.MustCompile(`^\S+@[A-Za-z][A-Za-z0-9_+.-]*:[A-Za-z0-9=_-]+$`)

// Issue is one failed Profile rule: the definition path and a short reason.
// Registration returns every Issue (spec 03 3.4 step 3).
type Issue struct {
	Path   string
	Reason string
}

func (i Issue) Error() string { return i.Path + ": " + strings.ReplaceAll(i.Reason, "_", " ") }

type issues []Issue

func (s *issues) add(path, reason string) { *s = append(*s, Issue{Path: path, Reason: reason}) }

// first keeps the error-returning API for callers that need only a verdict.
func (s issues) first() error {
	if len(s) == 0 {
		return nil
	}
	return s[0]
}

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

// profilePath accepts a path inside the same commit (contract 4.1): relative,
// without a ".." segment, NUL or backslash. A trailing "/" may name a
// directory.
func profilePath(value string) bool {
	if value == "" || path.IsAbs(value) || strings.ContainsAny(value, "\\\x00") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return false
		}
	}
	return path.Clean(value) != "."
}

// DecodeProfile decodes a Profile definition (contract 5章) and returns every
// rule failure that needs neither the Git repository nor deployment settings.
func DecodeProfile(data []byte) (ProfileDefinition, []Issue) {
	var found issues
	collectShape(data, reflect.TypeFor[ProfileDefinition](), "", func(p, reason string) {
		// work has no program-stage settings: report them as not allowed in
		// this stage rather than as unknown names (contract 5.2).
		if reason == "unknown_field" {
			switch p {
			case "stages.work.command", "stages.work.network", "stages.work.secrets", "stages.work.runtime":
				reason = "not_allowed_in_stage"
			}
		}
		found.add(p, reason)
	})
	type plain ProfileDefinition
	var value plain
	// Shape problems are already reported. Decode the rest leniently so the
	// rules below can report their own paths as well.
	_ = json.Unmarshal(data, &value)
	p := ProfileDefinition(value)
	shape := len(found)
	for _, issue := range p.issues() {
		if !underAny(issue.Path, found[:shape]) {
			found = append(found, issue)
		}
	}
	return p, dedupe(found)
}

// underAny reports whether path is inside a path with a shape problem, whose
// zero value would otherwise be reported again by the rules.
func underAny(path string, found []Issue) bool {
	for _, issue := range found {
		if strings.HasPrefix(path, issue.Path+".") || strings.HasPrefix(path, issue.Path+"[") {
			return true
		}
	}
	return false
}

// dedupe keeps the first reason for each path.
func dedupe(found []Issue) []Issue {
	seen := map[string]bool{}
	result := []Issue{}
	for _, issue := range found {
		if !seen[issue.Path] {
			seen[issue.Path] = true
			result = append(result, issue)
		}
	}
	return result
}

func (p ProfileDefinition) Validate() error { return p.issues().first() }

func (p ProfileDefinition) issues() issues {
	var found issues
	if p.ContractVersion != Version {
		found.add("contractVersion", "unsupported")
	}
	if !validSemver(p.ProfileVersion) {
		found.add("profileVersion", "invalid_format")
	}
	if strings.TrimSpace(p.Name) == "" {
		found.add("name", "required")
	}
	if boundedText(p.Description, 2000, "description") != nil {
		found.add("description", "too_long")
	}
	if p.AcceptanceCriteria != "required" && p.AcceptanceCriteria != "optional" {
		found.add("acceptanceCriteria", "invalid")
	}
	if p.Params != nil && p.Params.Schema != "" && !profilePath(p.Params.Schema) {
		found.add("params.schema", "outside_repository")
	}
	for _, s := range p.programStages() {
		if s.definition != nil {
			s.definition.issues(s.stage, &found)
		}
	}
	w := p.Stages.Work
	if w.MicroHarness.Validate() != nil {
		found.add("stages.work.microHarness", "unsupported")
	}
	if strings.TrimSpace(w.Model) == "" {
		found.add("stages.work.model", "required")
	}
	if w.TimeoutSec < 1 {
		found.add("stages.work.timeoutSec", "invalid")
	}
	if w.Resume != "" && w.Resume != "required" && w.Resume != "optional" {
		found.add("stages.work.resume", "invalid")
	}
	for _, f := range p.workFiles() {
		if !profilePath(f.Value) {
			found.add(f.Path, "outside_repository")
		}
	}
	if len(w.ToolPermissions) > 0 {
		if reason := toolPermissionsShape(w.ToolPermissions); reason != "" {
			found.add("stages.work.toolPermissions", reason)
		}
	}
	// Program stages must not run files handed to the work stage (contract 4.1).
	for _, s := range p.programStages() {
		if s.definition == nil || len(s.definition.Command) == 0 || !profilePath(s.definition.Command[0]) {
			continue
		}
		command := path.Clean(s.definition.Command[0])
		for _, f := range p.workFiles() {
			if !profilePath(f.Value) {
				continue
			}
			work := path.Clean(f.Value)
			if command == work || strings.HasPrefix(command, work+"/") {
				found.add(fmt.Sprintf("stages.%s.command[0]", s.stage), "inside_work_files")
				break
			}
		}
	}
	seen := map[string]bool{}
	for i, a := range p.Approvals {
		if !operationPattern.MatchString(a.Operation) {
			found.add(fmt.Sprintf("approvals[%d].operation", i), "invalid_format")
		} else if seen[a.Operation] {
			found.add(fmt.Sprintf("approvals[%d].operation", i), "duplicate")
		}
		seen[a.Operation] = true
	}
	if p.Records != nil {
		if p.Records.Pass.Recent < 0 || p.Records.Pass.Recent > 50 {
			found.add("records.pass.recent", "out_of_range")
		}
		for i, file := range p.Records.Keep {
			stage, rest, _ := strings.Cut(file, "/")
			if stage != "prepare" && stage != "check" && stage != "publish" || !profilePath(rest) {
				found.add(fmt.Sprintf("records.keep[%d]", i), "invalid_format")
			}
		}
	}
	return found
}

// PathReference is one repository path a definition refers to, with the
// definition path that declared it.
type PathReference struct {
	Path  string
	Value string
}

func (p ProfileDefinition) workFiles() []PathReference {
	var files []PathReference
	for _, group := range []struct {
		name  string
		files []string
	}{{"instructions", p.Stages.Work.Instructions}, {"skills", p.Stages.Work.Skills}, {"settings", p.Stages.Work.Settings}} {
		for i, value := range group.files {
			files = append(files, PathReference{fmt.Sprintf("stages.work.%s[%d]", group.name, i), value})
		}
	}
	return files
}

// ReferencedPaths lists every repository path the definition refers to
// (spec 03 3.4 参照するパス): the first element of each program stage's
// command, stages.work.instructions/skills/settings and params.schema.
func (p ProfileDefinition) ReferencedPaths() []PathReference {
	var refs []PathReference
	for _, s := range p.programStages() {
		if s.definition != nil && len(s.definition.Command) > 0 {
			refs = append(refs, PathReference{fmt.Sprintf("stages.%s.command[0]", s.stage), s.definition.Command[0]})
		}
	}
	refs = append(refs, p.workFiles()...)
	if p.Params != nil && p.Params.Schema != "" {
		refs = append(refs, PathReference{"params.schema", p.Params.Schema})
	}
	return refs
}

// toolPermissionsShape checks the common form of spec 08 3.9: an object with
// optional allow/deny arrays of nonempty strings. Whether the values can be
// expressed by the micro-harness is decided by its Adapter.
func toolPermissionsShape(raw json.RawMessage) string {
	fields, err := objectFields(raw, "toolPermissions")
	if err != nil {
		return "expected_object"
	}
	for key, value := range fields {
		if key != "allow" && key != "deny" {
			return "unconvertible"
		}
		var list []*string
		if json.Unmarshal(value, &list) != nil || list == nil {
			return "unconvertible"
		}
		for _, item := range list {
			if item == nil || *item == "" {
				return "unconvertible"
			}
		}
	}
	return ""
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

func (p ProgramStage) issues(stage model.Stage, found *issues) {
	prefix := "stages." + string(stage) + "."
	if len(p.Command) == 0 {
		found.add(prefix+"command", "required")
	} else if !profilePath(p.Command[0]) {
		found.add(prefix+"command[0]", "outside_repository")
	}
	if p.TimeoutSec < 1 {
		found.add(prefix+"timeoutSec", "invalid")
	}
	if stage == model.StageCheck {
		if p.Network != nil {
			found.add(prefix+"network", "not_allowed_in_stage")
		}
		if p.Secrets != nil {
			found.add(prefix+"secrets", "not_allowed_in_stage")
		}
		return
	}
	if p.Network != nil {
		seen := map[string]bool{}
		for i, dest := range p.Network.Allow {
			name := fmt.Sprintf("%snetwork.allow[%d]", prefix, i)
			if reason := destinationIssue(dest); reason != "" {
				found.add(name, reason)
			} else if seen[dest] {
				found.add(name, "duplicate")
			}
			seen[dest] = true
		}
	}
	seen := map[string]bool{}
	for i, secret := range p.Secrets {
		name := fmt.Sprintf("%ssecrets[%d]", prefix, i)
		if !slotPattern.MatchString(secret.Slot) {
			found.add(name+".slot", "invalid_format")
		} else if seen[secret.Slot] {
			found.add(name+".slot", "duplicate")
		}
		seen[secret.Slot] = true
		if secret.Access != "read" && secret.Access != "write" {
			found.add(name+".access", "invalid")
		} else if stage == model.StagePrepare && secret.Access != "read" {
			found.add(name+".access", "not_allowed_in_stage")
		}
	}
	p.runtimeIssues(prefix, found)
}

func (p ProgramStage) runtimeIssues(prefix string, found *issues) {
	if p.Runtime == nil {
		return
	}
	switch p.Runtime.Kind {
	case "container":
		if !digestPattern.MatchString(p.Runtime.Image) {
			found.add(prefix+"runtime.image", "digest_required")
		}
	case "macos_vm":
		if strings.TrimSpace(p.Runtime.Image) == "" {
			found.add(prefix+"runtime.image", "required")
		}
	default:
		found.add(prefix+"runtime.kind", "invalid")
	}
}

// destinationIssue checks a network.allow entry (contract 5.1, spec 03 3.4):
// a lowercase DNS host name and a port 1–65535 joined by ":". Wildcards, IP
// literals and local names (a single label, or a last label of localhost,
// local or internal) are rejected.
func destinationIssue(value string) string {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || strings.Contains(value, "[") {
		if strings.Contains(value, "[") || net.ParseIP(strings.Trim(host, "[]")) != nil {
			return "ip_literal"
		}
		return "invalid_format"
	}
	if net.ParseIP(host) != nil {
		return "ip_literal"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strings.Trim(port, "0123456789") != "" || port[0] == '0' {
		return "invalid_port"
	}
	if strings.Contains(host, "*") {
		return "wildcard"
	}
	labels := strings.Split(host, ".")
	last := labels[len(labels)-1]
	if len(labels) < 2 || last == "localhost" || last == "local" || last == "internal" {
		return "local_name"
	}
	if len(host) > 253 {
		return "invalid_format"
	}
	for _, label := range labels {
		if !hostnameLabelPattern.MatchString(label) {
			return "invalid_format"
		}
	}
	if strings.Trim(last, "0123456789") == "" {
		return "ip_literal"
	}
	return ""
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
	found := issues(p.RegistrationIssues(tagVersion, environment))
	return found.first()
}

// RegistrationIssues returns every failure of the definition rules, the tag
// version and the deployment environment (spec 03 3.4).
func (p ProfileDefinition) RegistrationIssues(tagVersion string, environment ProfileEnvironment) []Issue {
	return dedupe(append(p.issues(), p.EnvironmentIssues(tagVersion, environment)...))
}

// EnvironmentIssues returns only the tag version and environment failures.
func (p ProfileDefinition) EnvironmentIssues(tagVersion string, environment ProfileEnvironment) []Issue {
	var found issues
	if p.ProfileVersion != tagVersion {
		found.add("profileVersion", "differs_from_tag")
	}
	if environment == nil {
		found.add("$", "environment_unavailable")
		return found
	}
	if max := environment.StageTimeoutMaxSec(model.StageWork); max < 1 || p.Stages.Work.TimeoutSec > max {
		found.add("stages.work.timeoutSec", "exceeds_limit")
	}
	w := p.Stages.Work
	if len(w.ToolPermissions) > 0 && w.MicroHarness.Validate() == nil && toolPermissionsShape(w.ToolPermissions) == "" &&
		environment.ValidateToolPermissions(w.MicroHarness, w.ToolPermissions) != nil {
		found.add("stages.work.toolPermissions", "unconvertible")
	}
	for _, s := range p.programStages() {
		if s.definition == nil {
			continue
		}
		prefix := "stages." + string(s.stage) + "."
		if max := environment.StageTimeoutMaxSec(s.stage); max < 1 || s.definition.TimeoutSec > max {
			found.add(prefix+"timeoutSec", "exceeds_limit")
		}
		if s.definition.Runtime != nil && environment.ValidateRuntime(*s.definition.Runtime) != nil {
			found.add(prefix+"runtime.kind", "unsupported")
		}
		if s.definition.Network != nil && s.stage != model.StageCheck {
			for i, destination := range s.definition.Network.Allow {
				if destinationIssue(destination) == "" && environment.ValidateNetworkDestination(destination) != nil {
					found.add(fmt.Sprintf("%snetwork.allow[%d]", prefix, i), "model_api")
				}
			}
		}
	}
	return found
}
