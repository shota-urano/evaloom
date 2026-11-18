package securitytest

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
)

// Spec 14 3.3: the breakout test. The work stage (probe Adapter) writes an
// untrusted script into the workspace; the check stage runs it from there,
// and the prepare stage fetches the same script from the test destination
// and runs it. The script attempts the 3.2 accesses and the run channel's
// authorize. Everything here is test-owned, like the 3.2 oracle.

// RowAuthorize is the 3.3 row "authorize on the run channel".
const RowAuthorize = "authorize"

// BreakoutStages are the stages that run the untrusted script.
var BreakoutStages = []string{IsolationCheck, IsolationPrepare}

// BreakoutRows are the 3.3 rows realized with the 3.2 probe items, plus
// RowAuthorize. Environment, name resolution, profile and workspace rows
// are not part of 3.3.
var BreakoutRows = []string{RowControlAPI, RowManagementData, RowOwnerHome, RowOtherWorkspaces, RowProbeRead, RowProbeWrite,
	RowUndeclaredSecret, RowDeclaredDest, RowUndeclaredDest, RowModelAPI, RowAuthorize}

// BreakoutScriptName is the script's workspace-relative path (written by the
// work stage) and, under "/", its path on the test destination.
const BreakoutScriptName = "evaloom_breakout.py"

// BreakoutReport is the file the script writes in the stage's out directory.
func BreakoutReport(stage string) string { return "breakout-" + stage + ".json" }

// breakoutStage is replaced by the running stage's name (the script's first
// argument) so the trusted records name the stage that made a request.
const breakoutStage = "{{stage}}"

// breakoutPrefix makes probe.py a self-contained script: usage
// "python3 evaloom_breakout.py STAGE RESULT".
const breakoutPrefix = `import sys
_BREAKOUT_PLAN = %s
if len(sys.argv) != 3:
    raise SystemExit("usage: evaloom_breakout.py STAGE RESULT")
sys.argv[1:] = ["--inline-plan", _BREAKOUT_PLAN.replace(%q, sys.argv[1]), sys.argv[2]]
`

// Breakout is one 3.3 run: the script, the per-stage rules judging its
// report, and the stage plans of the probe harness and the probe Adapter.
type Breakout struct {
	Script []byte
	Rules  map[string][]IsolationRule // check, prepare
}

// NewBreakout builds the script for targets. rows, when non-empty,
// restricts the attempts to those rows (the fake-runtime run uses only the
// rows observable without a real Sandbox). The script carries no key value:
// key items hold only a hash and a length.
func NewBreakout(t IsolationTargets, rows ...string) (Breakout, error) {
	want := slices.Clone(BreakoutRows)
	if len(rows) != 0 {
		want = slices.DeleteFunc(want, func(row string) bool { return !slices.Contains(rows, row) })
	}
	// Every attempt is denied in an undeclared stage; per-stage exceptions
	// are applied to the rules below.
	plan, rules := IsolationPlan(breakoutStage, IsolationDeclarations{}, t, want...)
	var items []map[string]any
	if len(plan) != 0 && string(plan) != "null" {
		if err := json.Unmarshal(plan, &items); err != nil {
			return Breakout{}, err
		}
	}
	if slices.Contains(want, RowAuthorize) {
		id := RowAuthorize + "/channel"
		items = append(items, map[string]any{"id": id, "kind": "authorize", "operation": ProbeOperation,
			"contentId": strings.Repeat("0", 64), "timeoutSec": 10})
		rules = append(rules, IsolationRule{ID: id, Kind: "authorize", Row: RowAuthorize, Expect: Deny})
	}
	if len(items) == 0 {
		return Breakout{}, fmt.Errorf("securitytest: empty breakout plan")
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return Breakout{}, err
	}
	literal, err := json.Marshal(string(encoded)) // a JSON string is a Python string literal
	if err != nil {
		return Breakout{}, err
	}
	source, err := assets.ReadFile("assets/probe.py")
	if err != nil {
		return Breakout{}, err
	}
	b := Breakout{Script: append([]byte(fmt.Sprintf(breakoutPrefix, literal, breakoutStage)), source...), Rules: map[string][]IsolationRule{}}
	for _, stage := range BreakoutStages {
		staged := slices.Clone(rules)
		for i := range staged {
			r := &staged[i]
			// 3.3 asks the prepare stage only for the absence of probe_write
			// (probe_read may be declared there), and allows the declared
			// destination through the proxy (the script came from it).
			if stage == IsolationPrepare && (r.Row == RowProbeRead || r.ID == RowDeclaredDest+"/declared-proxy") {
				r.Expect = Either
			}
		}
		b.Rules[stage] = staged
	}
	return b, nil
}

// WorkPlan is the probe Adapter's work plan: write the script into the
// workspace (the Adapter runs with the workspace as working directory).
func (b Breakout) WorkPlan() json.RawMessage {
	plan, _ := json.Marshal([]map[string]any{{"id": "breakout/write", "kind": "write", "path": BreakoutScriptName, "value": string(b.Script)}})
	return plan
}

// StagePlans are the probe harness plans: check runs the script from the
// workspace; prepare fetches it from fetchURL (through the Sandbox's proxy
// settings when the stage has a declared network) and runs it. Each plan has
// one command item, "breakout/run", that must succeed.
func (b Breakout) StagePlans(fetchURL string) map[string]json.RawMessage {
	const fetch = `import subprocess, sys, urllib.request
with urllib.request.urlopen(sys.argv[1], timeout=20) as response:
    data = response.read()
with open(sys.argv[2], "wb") as target:
    target.write(data)
sys.exit(subprocess.run([sys.executable, sys.argv[2], sys.argv[3], sys.argv[4]]).returncode)
`
	run := func(argv ...string) json.RawMessage {
		plan, _ := json.Marshal([]map[string]any{{"id": "breakout/run", "kind": "command", "argv": argv, "timeoutSec": 300}})
		return plan
	}
	return map[string]json.RawMessage{
		IsolationCheck: run("python3", "{{workspace}}/"+BreakoutScriptName, IsolationCheck, "{{out}}/"+BreakoutReport(IsolationCheck)),
		IsolationPrepare: run("python3", "-c", fetch, fetchURL, "{{out}}/fetched-"+BreakoutScriptName, IsolationPrepare,
			"{{out}}/"+BreakoutReport(IsolationPrepare)),
	}
}

// BreakoutVerdict judges one stage of 3.3. stageReport is the probe
// harness's own report (out/probe-<stage>.json), which must show the script
// ran; report is the script's report. On top of ObservationVerdict (report
// and trusted records), an authorize attempt that never reached the channel
// is unavailable rather than a refusal, and any authorize request in the
// Control API access log is a violation (the probe harness asks none).
func BreakoutVerdict(stage string, stageReport, report []byte, rules []IsolationRule, d IsolationDeclarations, records TrustedRecords) []Violation {
	var out []Violation
	add := func(row, format string, v ...any) {
		out = append(out, Violation{Row: row, Stage: stage, Detail: fmt.Sprintf(format, v...)})
	}
	ran, err := probe.DecodeReport(stageReport)
	switch {
	case err != nil:
		add(RowEnvironment, "実行不能: stage report unavailable or invalid: %v", err)
	case len(ran) != 1 || ran[0].ID != "breakout/run" || !*ran[0].Succeeded:
		add(RowEnvironment, "実行不能: the stage did not run the breakout script: %+v", ran)
	}
	out = append(out, ObservationVerdict(stage, report, rules, d, records)...)
	if observations, err := probe.DecodeReport(report); err == nil {
		for _, o := range observations {
			if o.Kind != "authorize" {
				continue
			}
			detail, _ := o.Detail.(map[string]any)
			if connected, _ := detail["connected"].(bool); !connected {
				add(RowAuthorize, "実行不能: %s did not reach the run channel", o.ID)
			}
		}
	}
	for _, uri := range records.ControlAPI {
		if strings.Contains(uri, "/authorize") {
			add(RowAuthorize, "Control API received %s", uri)
		}
	}
	return out
}
