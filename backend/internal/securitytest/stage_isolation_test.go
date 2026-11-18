package securitytest

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/runner"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

var isolationPaths = contract.RequestPaths{Profile: "/evaloom/profile", Workspace: "/evaloom/workspace", Out: "/evaloom/out",
	Inputs: "/evaloom/inputs", Records: "/evaloom/records", Scratch: "/evaloom/scratch"}

func declaredEverything() IsolationDeclarations {
	return IsolationDeclarations{
		Secrets: map[string][]contract.Secret{
			"prepare": {{Slot: ProbeReadSlot, Access: "read"}},
			"publish": {{Slot: ProbeReadSlot, Access: "read"}, {Slot: ProbeWriteSlot, Access: "write"}},
		},
		Network:  map[string][]string{"prepare": {"probe-destination.test:8080"}, "publish": {"probe-destination.test:8080"}},
		ModelAPI: true,
	}
}

// The oracle is the 3.2 table itself. R = must succeed, - = must fail.
func TestIsolationTableMatchesSpec(t *testing.T) {
	table := map[string]string{ // prepare work check publish proposal
		RowControlAPI:       "-----",
		RowManagementData:   "-----",
		RowOwnerHome:        "-----",
		RowProbeRead:        "R--R-",
		RowProbeWrite:       "---R-",
		RowUndeclaredSecret: "-----",
		RowDeclaredDest:     "R--R-",
		RowUndeclaredDest:   "-----",
		RowModelAPI:         "-R--R",
		RowProfileRead:      "R-RR-",
		RowWorkspaceWrite:   "RR---",
		RowOtherWorkspaces:  "-----",
	}
	cells := func(d IsolationDeclarations, row string) string {
		var s strings.Builder
		for _, stage := range IsolationStages {
			if d.Expected(row, stage) == Require {
				s.WriteByte('R')
			} else {
				s.WriteByte('-')
			}
		}
		return s.String()
	}
	d := declaredEverything()
	for row, want := range table {
		if got := cells(d, row); got != want {
			t.Errorf("%s: %s; want %s", row, got, want)
		}
	}
	// "only when declared": nothing declared, nothing delivered or reachable.
	none := IsolationDeclarations{}
	for _, row := range []string{RowProbeRead, RowProbeWrite, RowDeclaredDest, RowModelAPI} {
		if got := cells(none, row); got != "-----" {
			t.Errorf("undeclared %s: %s", row, got)
		}
	}
	// probe_write declared as read for prepare is still absent.
	readWrite := IsolationDeclarations{Secrets: map[string][]contract.Secret{"prepare": {{Slot: ProbeWriteSlot, Access: "read"}}}}
	if readWrite.Expected(RowProbeWrite, IsolationPrepare) != Deny {
		t.Error("probe_write expected in prepare when declared read")
	}
	if got := d.EnvAllowlist(IsolationCheck); slices.Contains(got, SecretEnvName(ProbeReadSlot)) || slices.Contains(got, "HTTP_PROXY") {
		t.Errorf("check allowlist %v", got)
	}
	if got := d.EnvAllowlist(IsolationPublish); !slices.Contains(got, SecretEnvName(ProbeWriteSlot)) || !slices.Contains(got, "HTTPS_PROXY") {
		t.Errorf("publish allowlist %v", got)
	}
	if got := d.EnvAllowlist(IsolationProposal); slices.Contains(got, "EVALOOM_REQUEST") {
		t.Errorf("proposal allowlist %v", got)
	}
}

// isolatedArgs is a create command shaped like the Sandbox's, per stage.
func isolatedArgs(stage string, d IsolationDeclarations, host string) []string {
	args := []string{"container", "create", "--name", "evaloom-x", "--pull", "never", "--network", "none", "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--ipc", "private", "--init", "--no-healthcheck", "--entrypoint", "/usr/bin/env",
		"--tmpfs", "/evaloom/scratch/.home:rw"}
	if d.networked(stage) {
		args[7] = "container:evaloom-x-ingress"
	}
	mount := func(source, target string, ro bool) {
		m := "type=bind,source=" + host + source + ",target=" + target
		if ro {
			m += ",readonly"
		}
		args = append(args, "--mount", m)
	}
	attempt := host + "/data/executions/e1/" + stage + "-1"
	args = append(args, "--mount", "type=bind,source="+attempt+"/out,target=/evaloom/out")
	switch stage {
	case IsolationPrepare:
		mount("/data/profiles/p1", isolationPaths.Profile, true)
		mount("/ws/task-1", isolationPaths.Workspace, false)
	case IsolationWork:
		mount("/ws/task-1", isolationPaths.Workspace, false)
		mount("/data/executions/e1/records", isolationPaths.Records, true)
	case IsolationCheck, IsolationPublish:
		mount("/data/profiles/p1", isolationPaths.Profile, true)
		mount("/ws/task-1", isolationPaths.Workspace, true)
	}
	keys := d.ExpectedSecretEnv(stage)
	for _, name := range keys {
		args = append(args, "--env", name)
	}
	args = append(args, "image")
	if len(keys) != 0 {
		args = append(args, "-u", "HOSTNAME")
	} else {
		args = append(args, "-i")
	}
	args = append(args, "HOME=/evaloom/scratch/.home", "TZ=UTC", "PATH=/usr/bin:/bin", "LANG=C.UTF-8")
	switch stage {
	case IsolationWork:
		args = append(args, "EVALOOM_REQUEST=/evaloom/request.json", "CODEX_HOME=/evaloom/config")
	case IsolationProposal:
		args = append(args, "CODEX_HOME=/evaloom/config")
	default:
		args = append(args, "EVALOOM_REQUEST=/evaloom/request.json", "EVALOOM_RESULT=/evaloom/scratch/result.json", "EVALOOM_CHANNEL=/evaloom/channel.sock")
	}
	if d.networked(stage) {
		args = append(args, "HTTP_PROXY=http://127.0.0.1:18080", "NO_PROXY=")
	}
	return append(args, "bin/probe", stage)
}

func isolationWiring(host string) ArgsWiring {
	return ArgsWiring{Paths: isolationPaths,
		Protected: map[string][]string{
			RowManagementData:  {host + "/control.db", host + "/data/secrets", host + "/token"},
			RowOwnerHome:       {"/var/run/docker.sock"},
			RowOtherWorkspaces: {host + "/ws/task-2"},
		},
		Ancestors: map[string][]string{RowManagementData: {host + "/data"}, RowOwnerHome: {host + "/home"}},
		Secrets:   []string{"canary-read", "canary-write"}}
}

func TestCreateArgsVerdictRejectsLoosenedSandboxes(t *testing.T) {
	host := "/h"
	d := declaredEverything()
	w := isolationWiring(host)
	for _, stage := range IsolationStages {
		if v := CreateArgsVerdict(stage, isolatedArgs(stage, d, host), d, w); len(v) != 0 {
			t.Fatalf("%s isolated args rejected: %v", stage, v)
		}
	}
	for _, tc := range []struct {
		name, stage string
		loosen      func([]string) []string
		row         string
	}{
		{"loopback allowed", IsolationCheck, func(a []string) []string {
			return Loosening{Stage: IsolationCheck, Network: "host"}.Loosen(a, isolationPaths)
		}, RowControlAPI},
		{"key to work", IsolationWork, func(a []string) []string {
			return Loosening{Stage: IsolationWork, Env: map[string]string{SecretEnvName(ProbeReadSlot): "canary-read"}}.Loosen(a, isolationPaths)
		}, RowProbeRead},
		{"write key to prepare", IsolationPrepare, func(a []string) []string {
			return slices.Insert(a, 2, "--env", SecretEnvName(ProbeWriteSlot))
		}, RowProbeWrite},
		{"data directory mounted", IsolationPublish, func(a []string) []string {
			return Loosening{Stage: IsolationPublish, Mounts: []string{"type=bind,source=" + host + "/data,target=/data"}}.Loosen(a, isolationPaths)
		}, RowManagementData},
		{"home mounted", IsolationWork, func(a []string) []string {
			return slices.Insert(a, 2, "--mount", "type=bind,source="+host+",target=/h")
		}, RowOwnerHome},
		{"docker socket", IsolationPrepare, func(a []string) []string {
			return slices.Insert(a, 2, "--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock")
		}, RowOwnerHome},
		{"other workspace", IsolationCheck, func(a []string) []string {
			return slices.Insert(a, 2, "--mount", "type=bind,source="+host+"/ws/task-2,target=/other,readonly")
		}, RowOtherWorkspaces},
		{"profile to work", IsolationWork, func(a []string) []string {
			return slices.Insert(a, 2, "--mount", "type=bind,source="+host+"/data/profiles/p1,target=/evaloom/profile,readonly")
		}, RowProfileRead},
		{"writable workspace in check", IsolationCheck, func(a []string) []string {
			i := slices.Index(a, "type=bind,source="+host+"/ws/task-1,target=/evaloom/workspace,readonly")
			a[i] = strings.TrimSuffix(a[i], ",readonly")
			return a
		}, RowWorkspaceWrite},
		{"extra variable", IsolationProposal, func(a []string) []string {
			return Loosening{Stage: IsolationProposal, Env: map[string]string{"SSH_AUTH_SOCK": "/agent"}}.Loosen(a, isolationPaths)
		}, RowEnvironment},
		{"host alias", IsolationWork, func(a []string) []string {
			return slices.Insert(a, 2, "--add-host", "host.docker.internal:host-gateway")
		}, RowControlAPI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.loosen(isolatedArgs(tc.stage, d, host))
			v := CreateArgsVerdict(tc.stage, args, d, w)
			if !slices.Contains(Rows(v), tc.row) {
				t.Fatalf("loosened %s not detected as %s: %v", tc.stage, tc.row, v)
			}
		})
	}
	// Loosen touches only the named stage's command.
	args := isolatedArgs(IsolationCheck, d, host)
	if got := (Loosening{Stage: IsolationPublish, Network: "host"}).Loosen(args, isolationPaths); !slices.Equal(got, args) {
		t.Fatal("loosening applied to another stage")
	}
}

func observation(id, kind string, ok bool, detail any) map[string]any {
	return map[string]any{"id": id, "kind": kind, "succeeded": ok, "detail": detail}
}

// conformingReport answers every rule as the table expects.
func conformingReport(stage string, rules []IsolationRule, d IsolationDeclarations) []map[string]any {
	var obs []map[string]any
	for _, r := range rules {
		var detail any = map[string]any{}
		if r.Kind == "env" {
			detail = map[string]any{"names": []string{"PATH", "HOME"}, "unexpected": []string{}, "secretPresent": false}
		}
		obs = append(obs, observation(r.ID, r.Kind, r.Expect == Require, detail))
	}
	return obs
}

func encodeReport(t *testing.T, obs []map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"observations": obs})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func isolationTargets() IsolationTargets {
	return IsolationTargets{
		ControlAPI: []string{"http://127.0.0.1:1", "http://[::1]:1", "http://192.0.2.1:1"},
		Hidden: map[string][]string{RowManagementData: {"/h/control.db", "/h/token"}, RowOwnerHome: {"/h/home/.ssh"},
			RowOtherWorkspaces: {"/h/ws/task-2/marker"}},
		Declared: Endpoint{"probe-destination.test", 8080}, Undeclared: Endpoint{"probe-undeclared.test", 8080}, Model: Endpoint{"probe-model.test", 8443},
		Secrets: ProbeSecrets{Read: "canary-read", Write: "canary-write"}, UndeclaredSecret: "canary-other",
		ProfileFile: "/evaloom/profile/evaloom/profile.yaml", Workspace: "/evaloom/workspace",
	}
}

func TestObservationVerdictUsesReportAndTrustedRecords(t *testing.T) {
	d := declaredEverything()
	targets := isolationTargets()
	for _, stage := range IsolationStages {
		plan, rules := IsolationPlan(stage, d, targets)
		if strings.Contains(string(plan), "canary") {
			t.Fatalf("%s plan carries a key value", stage)
		}
		records := TrustedRecords{Destinations: map[string][]string{RowDeclaredDest: {}}}
		if d.Expected(RowDeclaredDest, stage) == Require {
			records.Destinations[RowDeclaredDest] = []string{IsolationProbePath + "?stage=" + stage + "&probe=" + RowDeclaredDest}
		}
		if v := ObservationVerdict(stage, encodeReport(t, conformingReport(stage, rules, d)), rules, d, records); len(v) != 0 {
			t.Fatalf("%s conforming report rejected: %v", stage, v)
		}
	}
	stage := IsolationWork
	_, rules := IsolationPlan(stage, d, targets)
	flip := func(row string) []map[string]any {
		obs := conformingReport(stage, rules, d)
		for _, o := range obs {
			if strings.HasPrefix(o["id"].(string), row+"/") {
				o["succeeded"] = !o["succeeded"].(bool)
			}
		}
		return obs
	}
	for _, row := range []string{RowControlAPI, RowManagementData, RowOwnerHome, RowProbeRead, RowProbeWrite, RowUndeclaredSecret, RowModelAPI, RowProfileRead, RowWorkspaceWrite, RowOtherWorkspaces, RowUndeclaredDest, RowDeclaredDest} {
		if v := ObservationVerdict(stage, encodeReport(t, flip(row)), rules, d, TrustedRecords{}); !slices.Contains(Rows(v), row) {
			t.Errorf("flipped %s not detected: %v", row, v)
		}
	}
	// The report claims isolation, but the trusted records disagree.
	records := TrustedRecords{ControlAPI: []string{"/api/tasks?stage=work&probe=owner"},
		Destinations: map[string][]string{RowUndeclaredDest: {IsolationProbePath + "?stage=work&probe=" + RowUndeclaredDest}}}
	v := ObservationVerdict(stage, encodeReport(t, conformingReport(stage, rules, d)), rules, d, records)
	if !slices.Contains(Rows(v), RowControlAPI) || !slices.Contains(Rows(v), RowUndeclaredDest) {
		t.Fatalf("trusted records ignored: %v", v)
	}
	// A variable outside the allowlist, even if the probe says nothing is unexpected.
	obs := conformingReport(stage, rules, d)
	for _, o := range obs {
		if o["kind"] == "env" {
			o["detail"] = map[string]any{"names": []string{"PATH", "SSH_AUTH_SOCK"}, "unexpected": []string{}, "secretPresent": false}
		}
	}
	if v := ObservationVerdict(stage, encodeReport(t, obs), rules, d, TrustedRecords{}); !slices.Contains(Rows(v), RowEnvironment) {
		t.Fatalf("variable not detected: %v", v)
	}
	// A missing observation is unavailable, never a pass.
	if v := ObservationVerdict(stage, encodeReport(t, conformingReport(stage, rules, d)[1:]), rules, d, TrustedRecords{}); len(v) == 0 || !strings.Contains(v[0].Detail, "実行不能") {
		t.Fatalf("missing observation accepted: %v", v)
	}
	if v := ObservationVerdict(stage, []byte(`{}`), rules, d, TrustedRecords{}); len(v) == 0 {
		t.Fatal("empty report accepted")
	}
}

// The probe's resolve kind and proxied http kind behave as the plan assumes:
// a proxy's 403 is not reaching the destination.
func TestProbeResolveAndProxiedHTTP(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Host, "allowed") {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "denied", http.StatusForbidden)
	})}
	go server.Serve(proxy)
	defer server.Close()
	dir := t.TempDir()
	source, err := assets.ReadFile("assets/probe.py")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "probe.py")
	if err := os.WriteFile(script, source, 0600); err != nil {
		t.Fatal(err)
	}
	plan, _ := json.Marshal([]any{
		map[string]any{"id": "resolve-ok", "kind": "resolve", "host": "localhost"},
		map[string]any{"id": "resolve-no", "kind": "resolve", "host": "evaloom-probe.invalid"},
		map[string]any{"id": "proxy-ok", "kind": "http", "proxy": true, "url": "http://allowed.test:8080" + IsolationProbePath, "timeoutSec": 3},
		map[string]any{"id": "proxy-denied", "kind": "http", "proxy": true, "url": "http://denied.test:8080" + IsolationProbePath, "timeoutSec": 3},
	})
	planPath, resultPath := filepath.Join(dir, "plan.json"), filepath.Join(dir, "result.json")
	if err := os.WriteFile(planPath, plan, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", script, planPath, resultPath)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HTTP_PROXY=http://" + proxy.Addr().String(), "http_proxy=http://" + proxy.Addr().String(), "PYTHONDONTWRITEBYTECODE=1"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe: %v %s", err, out)
	}
	got := readObservations(t, resultPath)
	if !got["resolve-ok"] || got["resolve-no"] || !got["proxy-ok"] || got["proxy-denied"] {
		t.Fatalf("observations %v", got)
	}
}

// fakeProposalRuntime records the workload's create command and plays the
// proxy sidecars' part well enough for the Sandbox to start. It proves the
// Proposal Sandbox's configuration, not its enforcement.
const fakeProposalRuntime = `#!/bin/sh
case "$1 $2" in
 'image inspect') printf '%s' '{"Os":"linux","Config":{}}' ;;
 'volume create'|'volume rm'|'container rm'|'container exec') ;;
 'volume ls') ;;
 'container create')
   name=$4
   case "$name" in *-ingress|*-egress) ;; *) printf '%s\n' "$@" > "$FAKE_STATE/workload" ;; esac ;;
 'container start') ;;
 'container inspect')
   for name; do :; done
   case "$name" in
    *-egress) printf '%s' '{"State":{"Running":true},"HostConfig":{"NetworkMode":"bridge"},"NetworkSettings":{"Networks":{"bridge":{}}}}' ;;
    *-ingress) printf '%s' '{"State":{"Running":true},"HostConfig":{"NetworkMode":"none"},"NetworkSettings":{"Networks":{"none":{}}}}' ;;
    *) printf '%s' '{"Status":"exited","Running":false,"ExitCode":0}' ;;
   esac ;;
 *) echo "unsupported $*" >&2; exit 99 ;;
esac
`

type modelDestinations []string

func (m modelDestinations) ModelAPIDestinations() []string { return m }

// Spec 14 3.2, Proposal column, as far as it is decided before the
// container runs: the production Proposal Sandbox spec (runner) through the
// production Container yields a create command that satisfies the table.
// The in-Sandbox attempts need a real container (security_docker tag).
func TestProposalSandboxCreateArgsIsolation(t *testing.T) {
	root, err := os.MkdirTemp("/private/tmp", "e7p-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	data := filepath.Join(root, "data")
	attempt := filepath.Join(data, "proposals", "j1", "proposal-1")
	dirs := map[string]string{"out": filepath.Join(attempt, "out"), "inputs": filepath.Join(attempt, "inputs"), "scratch": filepath.Join(attempt, "scratch"),
		"config": filepath.Join(attempt, "config", "c"), "state": filepath.Join(root, "state"), "secrets": filepath.Join(data, "secrets")}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	token, proxy, cli := filepath.Join(root, "token"), filepath.Join(root, "proxy"), filepath.Join(root, "runtime")
	for path, content := range map[string]string{token: "t", proxy: "#!/bin/sh\n", cli: fakeProposalRuntime} {
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	paths := runner.DefaultProposalPaths
	config := sandbox.ContainerConfig{RuntimePath: cli, Image: "probe-image", ProxyImage: "probe-proxy", ProxyBinary: proxy,
		RuntimeEnv: map[string]string{"FAKE_STATE": dirs["state"]}, StagePaths: paths,
		StageMountRoots: map[string]string{paths.Out: dirs["out"], paths.Inputs: dirs["inputs"], paths.Scratch: dirs["scratch"]},
		HostHome:        root + "/home", DataDir: data, RunnerTokenFile: token, ConfigDirRoot: filepath.Dir(dirs["config"]),
		ControlTimeout: 2 * time.Second, ModelAPI: modelDestinations{"probe-model.test:8443"}}
	if err := os.Mkdir(config.HostHome, 0700); err != nil {
		t.Fatal(err)
	}
	spec, err := runner.BuildProposalSandboxSpec(runner.ProposalHarnessInput{Paths: paths, InputsHost: dirs["inputs"], OutHost: dirs["out"], ScratchHost: dirs["scratch"],
		Env:    map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "CODEX_HOME": dirs["config"]},
		Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 1 << 28, Time: 10 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	spec.ConfigDir = dirs["config"]
	box, err := sandbox.NewContainer(config)
	if err != nil {
		t.Fatal(err)
	}
	process, err := box.RunControlled(t.Context(), spec, []string{"python3", "probe.py"})
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dirs["state"], "workload"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	d := IsolationDeclarations{ModelAPI: true}
	w := ArgsWiring{Paths: paths,
		Protected: map[string][]string{RowManagementData: {dirs["secrets"], token}},
		Ancestors: map[string][]string{RowManagementData: {data}, RowOwnerHome: {config.HostHome}}}
	parsed, err := ParseCreateArgs(args)
	if err != nil || StageOf(parsed, paths) != IsolationProposal {
		t.Fatalf("workload command not recognized: %v %v", err, args)
	}
	if v := CreateArgsVerdict(IsolationProposal, args, d, w); len(v) != 0 {
		t.Fatalf("Proposal Sandbox violates 3.2: %v\n%v", v, args)
	}
	// Loosened at the runtime CLI boundary: the verdict must fail.
	for row, l := range map[string]Loosening{
		RowControlAPI:     {Stage: IsolationProposal, Network: "host"},
		RowProbeRead:      {Stage: IsolationProposal, Env: map[string]string{SecretEnvName(ProbeReadSlot): "x"}},
		RowManagementData: {Stage: IsolationProposal, Mounts: []string{"type=bind,source=" + data + ",target=/data"}},
		RowWorkspaceWrite: {Stage: IsolationProposal, Mounts: []string{"type=bind,source=" + root + "/ws,target=" + paths.Workspace}},
		RowEnvironment:    {Stage: IsolationProposal, Env: map[string]string{"DOCKER_HOST": "unix:///var/run/docker.sock"}},
	} {
		if v := CreateArgsVerdict(IsolationProposal, l.Loosen(args, paths), d, w); !slices.Contains(Rows(v), row) {
			t.Errorf("loosened proposal (%s) not detected: %v", row, v)
		}
	}
}
