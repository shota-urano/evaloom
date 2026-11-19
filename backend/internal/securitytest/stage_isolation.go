package securitytest

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
	"github.com/shota-urano/evaloom/backend/internal/contract"
)

// Spec 14 3.2: the oracle of the per-stage isolation test. Everything here is
// test-owned (the table of the spec), never derived from the product wiring
// it checks. Two kinds of evidence are judged:
//
//   - CreateArgsVerdict judges the container runtime's create arguments the
//     Runner produced for one stage (what the Sandbox is configured to see,
//     write, reach and receive). It needs no container daemon.
//   - ObservationVerdict judges the probe's in-Sandbox access attempts
//     (IsolationPlan) together with trusted records outside the Sandbox
//     (the Control API access log and the test destinations' records).
//     Only a real Sandbox makes the file and network attempts meaningful.

// Isolation stages: the four Runner stages plus the Proposal Sandbox.
const (
	IsolationPrepare  = "prepare"
	IsolationWork     = "work"
	IsolationCheck    = "check"
	IsolationPublish  = "publish"
	IsolationProposal = "proposal"
)

// IsolationStages lists the columns of the 3.2 table.
var IsolationStages = []string{IsolationPrepare, IsolationWork, IsolationCheck, IsolationPublish, IsolationProposal}

// Rows of the 3.2 table. Violations name the row they break.
const (
	RowControlAPI       = "control_api"            // Control API (owner / Runner) on 127.0.0.1, ::1, host IP
	RowManagementData   = "management_data"        // management DB, data directory, Runner token
	RowOwnerHome        = "owner_home"             // owner HOME, keychain, SSH agent socket, Docker socket
	RowProbeRead        = "probe_read"             // probe_read value
	RowProbeWrite       = "probe_write"            // probe_write value
	RowUndeclaredSecret = "undeclared_secret"      // value of a stored but undeclared key
	RowEnvironment      = "environment"            // environment variable list
	RowDeclaredDest     = "declared_destination"   // declared destination
	RowUndeclaredDest   = "undeclared_destination" // undeclared destination
	RowModelAPI         = "model_api"              // model API destination
	RowNameResolution   = "name_resolution"        // DNS
	RowProfileRead      = "profile_read"           // reading profile
	RowWorkspaceWrite   = "workspace_write"        // writing the workspace
	RowOtherWorkspaces  = "other_workspaces"       // other tasks' workspaces and held sessions
	// The "model API credential" row applies only if Q8's first option is
	// taken; spec 09 3.5 decided the second option, so it has no probe.
)

// Outcome is the expected result of one access attempt.
type Outcome int

const (
	Deny    Outcome = iota // the attempt must fail
	Require                // the attempt must succeed (positive control)
	Either                 // not judged (for example DNS of a proxied, allowed name)
)

// IsolationDeclarations are the Profile's declarations that the 3.2 table
// depends on, plus whether the selected micro-harness Adapter declared a
// model API destination for work and proposal.
type IsolationDeclarations struct {
	Secrets  map[string][]contract.Secret // per program stage, as in the Profile
	Network  map[string][]string          // per program stage, network.allow
	ModelAPI bool                         // work/proposal get the model API destination
}

func (d IsolationDeclarations) declares(stage, slot string) bool {
	if stage != IsolationPrepare && stage != IsolationPublish {
		return false
	}
	return slices.ContainsFunc(d.Secrets[stage], func(s contract.Secret) bool { return s.Slot == slot })
}

func (d IsolationDeclarations) networked(stage string) bool {
	switch stage {
	case IsolationPrepare, IsolationPublish:
		return len(d.Network[stage]) != 0
	case IsolationWork, IsolationProposal:
		return d.ModelAPI
	}
	return false
}

// Expected returns the 3.2 table's cell for row and stage.
func (d IsolationDeclarations) Expected(row, stage string) Outcome {
	switch row {
	case RowProbeRead:
		// Prepare: only when declared (and stored) as read. Publish: when declared.
		if d.declares(stage, ProbeReadSlot) {
			return Require
		}
	case RowProbeWrite:
		// Even a prepare declaration with access: read never yields it.
		if stage == IsolationPublish && d.declares(stage, ProbeWriteSlot) {
			return Require
		}
	case RowDeclaredDest:
		if (stage == IsolationPrepare || stage == IsolationPublish) && d.networked(stage) {
			return Require
		}
	case RowModelAPI:
		if (stage == IsolationWork || stage == IsolationProposal) && d.ModelAPI {
			return Require
		}
	case RowProfileRead:
		if stage == IsolationPrepare || stage == IsolationCheck || stage == IsolationPublish {
			return Require
		}
	case RowWorkspaceWrite:
		if stage == IsolationPrepare || stage == IsolationWork {
			return Require
		}
	}
	return Deny
}

// SecretEnvName is the variable a declared slot is delivered in (spec 09 5).
func SecretEnvName(slot string) string { return "EVALOOM_SECRET_" + strings.ToUpper(slot) }

// proxyEnv are the Sandbox's proxy settings. Spec 09 3.3 realizes declared
// destinations through a proxy; contract 6.4 does not list these names, so
// they are accepted only when the stage has a declared network.
var proxyEnv = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy"}

// EnvAllowlist is the variable list allowed in stage (3.2 "environment"
// row): contract 6.4 plus declared keys for program stages, spec 09 5 for
// work and proposal.
func (d IsolationDeclarations) EnvAllowlist(stage string) []string {
	var names []string
	switch stage {
	case IsolationPrepare, IsolationCheck, IsolationPublish:
		names = []string{"EVALOOM_REQUEST", "EVALOOM_RESULT", "EVALOOM_CHANNEL", "HOME", "PATH", "LANG", "TZ"}
		for _, s := range d.Secrets[stage] {
			if d.declares(stage, s.Slot) {
				names = append(names, SecretEnvName(s.Slot))
			}
		}
	case IsolationWork:
		names = []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "HOME", "PATH", "LANG", "TZ", "EVALOOM_REQUEST"}
	case IsolationProposal:
		names = []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "HOME", "PATH", "LANG", "TZ"}
	}
	if d.networked(stage) {
		names = append(names, proxyEnv...)
	}
	return names
}

// ExpectedSecretEnv lists the key variables stage must receive. Prepare
// receives only read keys; the store refuses probe_write there, so a Profile
// that declares it fails before the Sandbox starts.
func (d IsolationDeclarations) ExpectedSecretEnv(stage string) []string {
	var names []string
	for _, slot := range []string{ProbeReadSlot, ProbeWriteSlot} {
		if (slot == ProbeReadSlot && d.Expected(RowProbeRead, stage) == Require) || (slot == ProbeWriteSlot && d.Expected(RowProbeWrite, stage) == Require) {
			names = append(names, SecretEnvName(slot))
		}
	}
	return names
}

// Violation is one broken cell of the 3.2 table.
type Violation struct {
	Row, Stage, Detail string
}

func (v Violation) String() string { return v.Stage + "/" + v.Row + ": " + v.Detail }

// Rows lists the distinct rows of violations.
func Rows(violations []Violation) []string {
	var rows []string
	for _, v := range violations {
		if !slices.Contains(rows, v.Row) {
			rows = append(rows, v.Row)
		}
	}
	slices.Sort(rows)
	return rows
}

// ---------------------------------------------------------------------------
// Container create arguments

// CreateMount is one --mount of a container create command.
type CreateMount struct {
	Type, Source, Target string
	ReadOnly             bool
}

// CreateArgs is a parsed "container create" command line.
type CreateArgs struct {
	Name     string
	Network  string
	Mounts   []CreateMount
	Imported []string            // --env NAME (value from the create client)
	Flags    map[string][]string // every other option
	Image    string
	ClearEnv bool     // env -i
	Unset    []string // env -u NAME
	Env      map[string]string
	Command  []string
}

var booleanFlags = []string{"--read-only", "--init", "--no-healthcheck", "--privileged", "--rm", "--interactive", "--tty", "-i", "-t"}

// ParseCreateArgs parses the argument vector given to the runtime CLI
// (starting with "container", "create").
func ParseCreateArgs(args []string) (CreateArgs, error) {
	if len(args) < 3 || args[0] != "container" || args[1] != "create" {
		return CreateArgs{}, fmt.Errorf("securitytest: not a container create command")
	}
	c := CreateArgs{Flags: map[string][]string{}, Env: map[string]string{}}
	i := 2
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		flag, value, inline := strings.Cut(args[i], "=")
		if slices.Contains(booleanFlags, flag) {
			c.Flags[flag] = append(c.Flags[flag], value)
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				return CreateArgs{}, fmt.Errorf("securitytest: option %s without value", flag)
			}
			i++
			value = args[i]
		}
		switch flag {
		case "--name":
			c.Name = value
		case "--network", "--net":
			c.Network = value
		case "--mount":
			m := CreateMount{}
			for _, part := range strings.Split(value, ",") {
				key, v, _ := strings.Cut(part, "=")
				switch key {
				case "type":
					m.Type = v
				case "source", "src":
					m.Source = v
				case "target", "destination", "dst":
					m.Target = v
				case "readonly", "ro":
					m.ReadOnly = v == "" || v == "true" || v == "1"
				}
			}
			c.Mounts = append(c.Mounts, m)
		case "--env", "-e":
			if strings.Contains(value, "=") {
				key, v, _ := strings.Cut(value, "=")
				c.Env[key] = v
			} else {
				c.Imported = append(c.Imported, value)
			}
		default:
			c.Flags[flag] = append(c.Flags[flag], value)
		}
	}
	if i >= len(args) {
		return CreateArgs{}, fmt.Errorf("securitytest: no image")
	}
	c.Image = args[i]
	i++
	// The entrypoint is /usr/bin/env: "-i" or "-u NAME" pairs, then NAME=VALUE.
	for options := true; options && i < len(args); {
		switch {
		case args[i] == "-i":
			c.ClearEnv = true
			i++
		case args[i] == "-u" && i+1 < len(args):
			c.Unset = append(c.Unset, args[i+1])
			i += 2
		default:
			options = false
		}
	}
	for ; i < len(args) && strings.Contains(args[i], "="); i++ {
		key, v, _ := strings.Cut(args[i], "=")
		c.Env[key] = v
	}
	c.Command = append([]string(nil), args[i:]...)
	return c, nil
}

// EnvStart is the index in args where the entrypoint's NAME=VALUE list
// starts (after the image and "-i" or the "-u NAME" pairs).
func envStart(args []string) int {
	i := 2
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		flag, _, inline := strings.Cut(args[i], "=")
		if !slices.Contains(booleanFlags, flag) && !inline {
			i++
		}
	}
	i++ // image
	for i < len(args) {
		if args[i] == "-i" {
			i++
		} else if args[i] == "-u" && i+1 < len(args) {
			i += 2
		} else {
			break
		}
	}
	return i
}

// StageOf names the Runner stage of a create command from its out mount
// source (<dataDir>/executions/<id>/<stage>-<attempt>/out). It returns ""
// when the command is not a stage container (for example a helper).
func StageOf(c CreateArgs, paths contract.RequestPaths) string {
	for _, m := range c.Mounts {
		if m.Target == paths.Out {
			// ExecuteProposal uses proposals/<id>/out, without a stage-attempt
			// directory (proposals are not Execution stages).
			if filepath.Base(filepath.Dir(filepath.Dir(m.Source))) == "proposals" {
				return IsolationProposal
			}
			attempt := filepath.Base(filepath.Dir(m.Source))
			stage, _, _ := strings.Cut(attempt, "-")
			return stage
		}
	}
	return ""
}

// Loosening deliberately weakens one stage's create command at the runtime
// CLI boundary, the way a broken Sandbox configuration would. It exists only
// to prove the verdicts fail (spec 14 7j6.2 acceptance); the product has no
// switch for it.
type Loosening struct {
	Stage   string            `json:"stage"`
	Network string            `json:"network,omitempty"` // replace --network (for example "host")
	Env     map[string]string `json:"env,omitempty"`     // add NAME=VALUE to the entrypoint env
	Mounts  []string          `json:"mounts,omitempty"`  // add --mount values
}

// Loosen returns args with l applied when the command belongs to l.Stage.
func (l Loosening) Loosen(args []string, paths contract.RequestPaths) []string {
	parsed, err := ParseCreateArgs(args)
	if err != nil || StageOf(parsed, paths) != l.Stage {
		return args
	}
	out := slices.Clone(args)
	if l.Network != "" {
		for i := 2; i+1 < len(out); i++ {
			if out[i] == "--network" {
				out[i+1] = l.Network
				break
			}
		}
	}
	if len(l.Env) != 0 {
		start := envStart(out)
		var vars []string
		for key, value := range l.Env {
			vars = append(vars, key+"="+value)
		}
		slices.Sort(vars)
		out = slices.Insert(out, start, vars...)
	}
	for _, mount := range l.Mounts {
		out = slices.Insert(out, 2, "--mount", mount)
	}
	return out
}

// ArgsWiring is the trusted context of a create-args verdict.
type ArgsWiring struct {
	Paths contract.RequestPaths // guest paths
	// Protected host paths: no mount source may contain, equal or lie
	// inside them (management DB, secret store, Runner token, logs,
	// artifacts, authentication files, other workspaces, sockets).
	Protected map[string][]string // row -> paths
	// Ancestors host paths a mount source must not contain or equal (the
	// owner HOME and the data directory, whose dedicated subtrees are shared).
	Ancestors map[string][]string // row -> paths
	// Secrets are the canary values; no create argument may carry one.
	Secrets []string
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// CreateArgsVerdict judges one stage's container create command against
// the 3.2 table. The trusted values (declarations, wiring) come from the test.
func CreateArgsVerdict(stage string, args []string, d IsolationDeclarations, w ArgsWiring) []Violation {
	var out []Violation
	add := func(row, format string, v ...any) {
		out = append(out, Violation{Row: row, Stage: stage, Detail: fmt.Sprintf(format, v...)})
	}
	c, err := ParseCreateArgs(args)
	if err != nil {
		add(RowManagementData, "実行不能: %v", err)
		return out
	}
	// Network: offline, or the proxy namespace when the table allows a destination.
	switch {
	case d.networked(stage):
		if !strings.HasPrefix(c.Network, "container:") {
			add(RowControlAPI, "network %q is not the proxy namespace", c.Network)
		}
	case c.Network != "none":
		add(RowControlAPI, "network %q; want none", c.Network)
		add(RowUndeclaredDest, "network %q; want none", c.Network)
		add(RowNameResolution, "network %q; want none", c.Network)
	}
	for _, flag := range []string{"--add-host", "--publish", "-p", "--dns", "--network-alias", "--pid", "--uts", "--userns"} {
		if len(c.Flags[flag]) != 0 {
			add(RowControlAPI, "option %s %v", flag, c.Flags[flag])
		}
	}
	if values, ok := c.Flags["--ipc"]; ok && !slices.Equal(values, []string{"private"}) {
		add(RowManagementData, "ipc %v", values)
	}
	for _, flag := range []string{"--privileged", "--device", "--volume", "-v", "--volumes-from", "--cap-add"} {
		if len(c.Flags[flag]) != 0 {
			add(RowManagementData, "option %s %v", flag, c.Flags[flag])
		}
	}
	// Mounts.
	targets := map[string]CreateMount{}
	for _, m := range c.Mounts {
		targets[m.Target] = m
		if m.Type == "bind" {
			for row, paths := range w.Protected {
				for _, p := range paths {
					if within(m.Source, p) || within(p, m.Source) {
						add(row, "mount %s exposes %s", m.Source, p)
					}
				}
			}
			for row, paths := range w.Ancestors {
				for _, p := range paths {
					if within(m.Source, p) {
						add(row, "mount %s contains %s", m.Source, p)
					}
				}
			}
		}
		if strings.Contains(m.Target, "docker.sock") || strings.Contains(m.Source, "docker.sock") || strings.Contains(m.Source, "ssh") {
			add(RowOwnerHome, "socket mount %s", m.Source)
		}
	}
	profile, hasProfile := targets[w.Paths.Profile]
	if d.Expected(RowProfileRead, stage) == Require {
		if !hasProfile || !profile.ReadOnly {
			add(RowProfileRead, "profile not mounted read-only")
		}
	} else if hasProfile {
		add(RowProfileRead, "profile visible")
	}
	workspace, hasWorkspace := targets[w.Paths.Workspace]
	switch {
	case d.Expected(RowWorkspaceWrite, stage) == Require:
		if !hasWorkspace || workspace.ReadOnly {
			add(RowWorkspaceWrite, "workspace not writable")
		}
	case stage == IsolationProposal:
		if hasWorkspace {
			add(RowWorkspaceWrite, "workspace visible to proposal")
		}
	case !hasWorkspace || !workspace.ReadOnly:
		add(RowWorkspaceWrite, "workspace not read-only")
	}
	// Keys and environment.
	secretRow := map[string]string{SecretEnvName(ProbeReadSlot): RowProbeRead, SecretEnvName(ProbeWriteSlot): RowProbeWrite}
	expectedSecrets := d.ExpectedSecretEnv(stage)
	for _, name := range c.Imported {
		if !slices.Contains(expectedSecrets, name) {
			row := secretRow[name]
			if row == "" {
				row = RowUndeclaredSecret
			}
			add(row, "key %s delivered", name)
		}
	}
	for _, name := range expectedSecrets {
		if !slices.Contains(c.Imported, name) {
			add(secretRow[name], "declared key %s missing", name)
		}
	}
	if !c.ClearEnv && len(c.Imported) == 0 {
		add(RowEnvironment, "entrypoint does not clear the environment")
	}
	if len(c.Imported) != 0 && len(c.Unset) == 0 {
		add(RowEnvironment, "keyed entrypoint does not unset image variables")
	}
	allow := d.EnvAllowlist(stage)
	for key := range c.Env {
		if strings.HasPrefix(key, "EVALOOM_SECRET_") {
			row := secretRow[key]
			if row == "" {
				row = RowUndeclaredSecret
			}
			add(row, "key variable %s set in arguments", key)
		}
		if !slices.Contains(allow, key) {
			add(RowEnvironment, "variable %s", key)
		}
	}
	for _, value := range w.Secrets {
		for _, arg := range args {
			if value != "" && strings.Contains(arg, value) {
				add(RowUndeclaredSecret, "a key value appears in the create arguments")
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// In-Sandbox probe plan and observations

// Endpoint is a DNS host and port of a test destination.
type Endpoint struct {
	Host string
	Port int
}

func (e Endpoint) Authority() string { return net.JoinHostPort(e.Host, strconv.Itoa(e.Port)) }

// IsolationProbePath is served (200) by every test destination; a request
// for it carries the stage and probe ID so the destination's record names it.
const IsolationProbePath = "/isolation-probe"

// IsolationTargets are the concrete things the probe attempts, all test
// owned. Paths are as seen from inside the Sandbox (for host paths, the host
// path itself: a correct Sandbox does not expose it).
type IsolationTargets struct {
	ControlAPI       []string            // base URLs, e.g. http://127.0.0.1:PORT
	Hidden           map[string][]string // RowManagementData / RowOwnerHome / RowOtherWorkspaces -> paths
	Declared         Endpoint
	Undeclared       Endpoint
	Model            Endpoint
	Secrets          ProbeSecrets
	UndeclaredSecret string // value of a stored but undeclared key
	// SecretFiles are searched for key values in addition to the environment.
	SecretFiles []string
	ProfileFile string // a committed Profile file, guest path
	Workspace   string // guest workspace path; the probe appends a marker file
}

// IsolationRule is the oracle for one plan item.
type IsolationRule struct {
	ID, Kind, Row string
	Expect        Outcome
}

// IsolationPlan returns the probe plan for stage and its rules. rows, when
// non-empty, restricts the plan to those rows.
func IsolationPlan(stage string, d IsolationDeclarations, t IsolationTargets, rows ...string) (json.RawMessage, []IsolationRule) {
	var items []map[string]any
	var rules []IsolationRule
	want := func(row string) bool { return len(rows) == 0 || slices.Contains(rows, row) }
	item := func(row, id string, expect Outcome, fields map[string]any) {
		if !want(row) {
			return
		}
		id = row + "/" + id
		fields["id"] = id
		items = append(items, fields)
		rules = append(rules, IsolationRule{ID: id, Kind: fields["kind"].(string), Row: row, Expect: expect})
	}
	marker := "?stage=" + stage + "&probe="
	for i, base := range t.ControlAPI {
		item(RowControlAPI, "owner-"+strconv.Itoa(i), Deny, map[string]any{"kind": "http", "url": base + "/api/tasks" + marker + "owner", "timeoutSec": 3})
		item(RowControlAPI, "runner-"+strconv.Itoa(i), Deny, map[string]any{"kind": "http", "method": "POST", "url": base + "/api/runner/claim" + marker + "runner", "timeoutSec": 3})
	}
	for _, row := range []string{RowManagementData, RowOwnerHome, RowOtherWorkspaces} {
		for i, path := range t.Hidden[row] {
			item(row, "exists-"+strconv.Itoa(i), Deny, map[string]any{"kind": "exists", "path": path})
			item(row, "read-"+strconv.Itoa(i), Deny, map[string]any{"kind": "read", "path": path})
		}
	}
	files := append([]string{}, t.SecretFiles...)
	item(RowProbeRead, "value", d.Expected(RowProbeRead, stage), SecretProbe("", t.Secrets.Read, files...))
	item(RowProbeWrite, "value", d.Expected(RowProbeWrite, stage), SecretProbe("", t.Secrets.Write, files...))
	if t.UndeclaredSecret != "" {
		item(RowUndeclaredSecret, "value", Deny, SecretProbe("", t.UndeclaredSecret, files...))
	}
	item(RowEnvironment, "names", Either, map[string]any{"kind": "env", "allowlist": d.EnvAllowlist(stage), "secrets": []string{}})
	dest := func(row, id string, e Endpoint, expect Outcome) {
		if e.Host == "" {
			return
		}
		url := "http://" + e.Authority() + IsolationProbePath + marker + row
		item(row, id+"-proxy", expect, map[string]any{"kind": "http", "proxy": true, "url": url, "timeoutSec": 5})
		item(row, id+"-direct", Deny, map[string]any{"kind": "tcp", "host": e.Host, "port": e.Port, "timeoutSec": 3})
	}
	dest(RowDeclaredDest, "declared", t.Declared, d.Expected(RowDeclaredDest, stage))
	dest(RowUndeclaredDest, "undeclared", t.Undeclared, Deny)
	dest(RowModelAPI, "model", t.Model, d.Expected(RowModelAPI, stage))
	resolve := func(id string, e Endpoint, allowed bool) {
		if e.Host == "" {
			return
		}
		expect := Deny
		if allowed {
			// Allowed names may be resolved by the proxy rather than in the
			// Sandbox (spec 09 3.3); either is consistent with the table.
			expect = Either
		}
		item(RowNameResolution, id, expect, map[string]any{"kind": "resolve", "host": e.Host})
	}
	resolve("declared", t.Declared, d.Expected(RowDeclaredDest, stage) == Require)
	resolve("undeclared", t.Undeclared, false)
	resolve("model", t.Model, d.Expected(RowModelAPI, stage) == Require)
	if t.ProfileFile != "" {
		item(RowProfileRead, "read", d.Expected(RowProfileRead, stage), map[string]any{"kind": "read", "path": t.ProfileFile})
	}
	if t.Workspace != "" {
		item(RowWorkspaceWrite, "write", d.Expected(RowWorkspaceWrite, stage), map[string]any{"kind": "write", "path": t.Workspace + "/.isolation-" + stage, "value": stage})
	}
	plan, _ := json.Marshal(items)
	return plan, rules
}

// TrustedRecords are requests received outside the Sandbox: the Control API
// access log and the test destinations' records, as request URIs or paths
// that carry the probe marker.
type TrustedRecords struct {
	ControlAPI   []string
	Destinations map[string][]string // RowDeclaredDest / RowUndeclaredDest / RowModelAPI -> request paths+queries
}

// ObservationVerdict judges the report the probe wrote for stage against
// rules, and the trusted records: any record of a "denied" attempt from
// stage is a violation even when the report claims a failure.
func ObservationVerdict(stage string, report []byte, rules []IsolationRule, d IsolationDeclarations, records TrustedRecords) []Violation {
	var out []Violation
	add := func(row, format string, v ...any) {
		out = append(out, Violation{Row: row, Stage: stage, Detail: fmt.Sprintf(format, v...)})
	}
	observations, err := probe.DecodeReport(report)
	if err != nil {
		add(RowEnvironment, "実行不能: report unavailable or invalid: %v", err)
		return out
	}
	byID := map[string]probe.Observation{}
	for _, o := range observations {
		byID[o.ID] = o
	}
	if len(observations) != len(rules) {
		add(RowEnvironment, "実行不能: %d observations for %d rules", len(observations), len(rules))
	}
	for _, rule := range rules {
		o, ok := byID[rule.ID]
		if !ok || o.Kind != rule.Kind {
			add(rule.Row, "実行不能: %s missing or wrong kind", rule.ID)
			continue
		}
		switch {
		case rule.Expect == Deny && *o.Succeeded:
			add(rule.Row, "%s succeeded", rule.ID)
		case rule.Expect == Require && !*o.Succeeded:
			add(rule.Row, "%s failed (positive control)", rule.ID)
		}
		if rule.Kind == "env" {
			encoded, _ := json.Marshal(o.Detail)
			var detail struct {
				Names      []string `json:"names"`
				Unexpected []string `json:"unexpected"`
			}
			if json.Unmarshal(encoded, &detail) != nil || len(detail.Names) == 0 || detail.Unexpected == nil {
				add(rule.Row, "実行不能: invalid environment evidence")
				continue
			}
			allow := d.EnvAllowlist(stage)
			for _, name := range detail.Names {
				if !slices.Contains(allow, name) {
					add(rule.Row, "variable %s", name)
				}
			}
		}
	}
	tag := "stage=" + stage + "&"
	for _, uri := range records.ControlAPI {
		if strings.Contains(uri, tag) {
			add(RowControlAPI, "Control API received %s", uri)
		}
	}
	for row, uris := range records.Destinations {
		allowed := d.Expected(row, stage) == Require
		for _, uri := range uris {
			if strings.Contains(uri, tag) && !allowed {
				add(row, "destination received %s", uri)
			}
		}
	}
	return out
}
