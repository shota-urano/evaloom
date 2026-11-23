package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// Task text of spec 09 3.3 "タスクのリンク": title, body and criteria, with
// links that must be dropped (IP, local names, ports, user information and
// the codex Adapter's model API hosts).
const (
	linkTitle = "調べる https://example.com/a。"
	linkBody  = "参照 (https://docs.example.org/x), <https://example.com/a> と https://b.example.net/p?q=1#f.\n" +
		"'https://quote.example.com/q' \"https://dq.example.com/\" [https://br.example.com/] https://trail.example.com/t.,、。\n" +
		"http://plain.example.com/ https://192.0.2.1/ https://[2001:db8::1]/ https://localhost/ https://intranet/ https://svc.local/ " +
		"https://build.internal/ https://example.com:8443/ https://explicit.example.com:443/ok https://user@cred.example.com/ " +
		"https://u:p@cred2.example.com/ https://api.openai.com/v1 https://chatgpt.com/x"
	linkCriteria = "https://docs.example.org/x を読む　https://ac.example.com/z"
)

var wantTaskLinks = []string{
	"https://example.com/a", "https://docs.example.org/x", "https://b.example.net/p?q=1#f",
	"https://quote.example.com/q", "https://dq.example.com/", "https://br.example.com/",
	"https://trail.example.com/t", "https://explicit.example.com:443/ok", "https://ac.example.com/z",
}

var wantLinkDestinations = []string{
	"example.com:443", "docs.example.org:443", "b.example.net:443", "quote.example.com:443", "dq.example.com:443",
	"br.example.com:443", "trail.example.com:443", "explicit.example.com:443", "ac.example.com:443",
}

// taskLinksFixture declares prepare network as given and registers the real
// codex Adapter, so the Q146 list comes from its declaration.
func taskLinksFixture(t *testing.T, network *contract.Network, title, body, criteria string) (*Runner, Claim, *stageReportStub, StageServices) {
	t.Helper()
	r, claim, reports, services := stageFixture(t, true)
	var def contract.ProfileDefinition
	if err := json.Unmarshal(claim.Profile.Definition, &def); err != nil {
		t.Fatal(err)
	}
	def.Stages.Prepare.Network = network
	def.Stages.Publish.Network = &contract.Network{Allow: []string{"api.github.com:443"}}
	data, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if _, issues := contract.DecodeProfile(data); len(issues) != 0 {
		t.Fatalf("invalid fixture definition: %v", issues)
	}
	claim.Profile.Definition = data
	claim.Task.Title, claim.Task.Body, claim.Task.AcceptanceCriteria = title, body, criteria
	services.Adapters = &adapter.Registry{}
	if err := services.Adapters.Register("codex", codex.New(codex.Config{})); err != nil {
		t.Fatal(err)
	}
	return r, claim, reports, services
}

func requestFields(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	readStageRequest(t, raw) // the strict contract 6.1 decoder accepts it
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestTaskLinksInPrepareRequestOnly(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name     string
		network  *contract.Network
		title    string
		body     string
		criteria string
		want     []string // nil: the field must be absent
	}{
		{"declared", &contract.Network{Allow: []string{"github.com:443"}, TaskLinks: &yes}, linkTitle, linkBody, linkCriteria, wantTaskLinks},
		{"declared without links", &contract.Network{TaskLinks: &yes}, "no links", "http://plain.example.com/ only", "", []string{}},
		{"not declared", &contract.Network{Allow: []string{"github.com:443"}}, linkTitle, linkBody, linkCriteria, nil},
		{"declared false", &contract.Network{Allow: []string{"github.com:443"}, TaskLinks: &no}, linkTitle, linkBody, linkCriteria, nil},
		{"no network", nil, linkTitle, linkBody, linkCriteria, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, claim, reports, services := taskLinksFixture(t, tc.network, tc.title, tc.body, tc.criteria)
			if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reports.started, executionStageOrder) {
				t.Fatalf("stages=%v", reports.started)
			}
			for _, stage := range executionStageOrder {
				raw, present := requestFields(t, reports.requests[stage])["taskLinks"]
				if stage != model.StagePrepare || tc.want == nil {
					if present {
						t.Fatalf("%s request has taskLinks: %s", stage, raw)
					}
					continue
				}
				var got []string
				if !present || json.Unmarshal(raw, &got) != nil || got == nil || !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("prepare taskLinks=%s; want %q", raw, tc.want)
				}
			}
			if tc.want != nil && len(tc.want) == 0 && !strings.Contains(string(reports.requests[model.StagePrepare]), `"taskLinks":[]`) {
				t.Fatalf("empty taskLinks is not an empty array: %s", reports.requests[model.StagePrepare])
			}
		})
	}
}

// fakeProxyRuntime appends each container create's arguments and serves the
// proxy sidecars' inspection.
func fakeProxyRuntime(t *testing.T, config *Config) string {
	t.Helper()
	state := t.TempDir()
	config.Container.RuntimePath = filepath.Join(state, "runtime")
	config.Container.RuntimeEnv = map[string]string{"LINKS_STATE": state}
	if err := os.WriteFile(config.Container.RuntimePath, []byte(`#!/bin/sh
for last; do :; done
case "$1 $2" in
 'image inspect') printf '%s' '{"Os":"linux","Config":{}}' ;;
 'container create') { printf '%s\n' "$@"; printf '%s\n' '----'; } >> "$LINKS_STATE/creates" ;;
 'container inspect')
  case "$last" in
   *-ingress) printf '%s' '{"State":{"Running":true},"HostConfig":{"NetworkMode":"none"},"NetworkSettings":{"Networks":{"none":{}}}}'; exit 0 ;;
   *-egress) printf '%s' '{"State":{"Running":true},"HostConfig":{"NetworkMode":"bridge"},"NetworkSettings":{"Networks":{"bridge":{}}}}'; exit 0 ;;
  esac
  printf '%s' '{"Status":"exited","Running":false,"ExitCode":0}' ;;
 'container start'|'container rm'|'container exec'|'volume create'|'volume rm') ;;
 *) exit 99 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	config.Container.ProxyBinary = filepath.Join(state, "proxy")
	if err := os.WriteFile(config.Container.ProxyBinary, []byte("fixture proxy"), 0700); err != nil {
		t.Fatal(err)
	}
	return state
}

type stageCreates struct {
	spec    sandbox.SandboxSpec
	creates [][]string
}

// egressDestinations returns the destinations the proxy is started with.
func egressDestinations(creates [][]string) ([]string, bool) {
	for _, args := range creates {
		i := slices.Index(args, "egress")
		if i < 0 || !slices.Contains(args, "/evaloom-proxy") {
			continue
		}
		var out []string
		for _, arg := range args[i+2:] {
			if strings.HasPrefix(arg, "--deny-ip=") {
				break
			}
			out = append(out, arg)
		}
		return out, true
	}
	return nil, false
}

// runStagesThroughContainers runs every stage's production sandbox assembly
// through the real container supervisor against the fake runtime.
func runStagesThroughContainers(t *testing.T, network *contract.Network, title, body, criteria string) map[model.Stage]stageCreates {
	t.Helper()
	r, claim, _, services := taskLinksFixture(t, network, title, body, criteria)
	config := testConfig(t, "https://control.example.test")
	config.DataDir = r.dataDir
	state := fakeProxyRuntime(t, &config)
	e := &executionExecutor{runner: r, config: config, claim: claim, paths: services.Paths, adapters: services.Adapters}
	got := map[model.Stage]stageCreates{}
	services.Executor = stageExecutorFunc(func(ctx context.Context, in StageInvocation) StageOutcome {
		os.Remove(filepath.Join(state, "creates"))
		spec, config, err := e.stageSandbox(in)
		if err != nil {
			t.Fatal(err)
		}
		if spec, err = in.SandboxSpec(spec); err != nil {
			t.Fatal(err)
		}
		box, err := sandbox.NewContainer(config)
		if err != nil {
			t.Fatal(err)
		}
		process, err := box.RunControlled(ctx, spec, []string{"fixture"})
		if err != nil {
			t.Fatalf("%s: %v", in.Stage, err)
		}
		if err := process.Wait(); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(state, "creates"))
		if err != nil {
			t.Fatal(err)
		}
		var creates [][]string
		for _, block := range strings.Split(strings.TrimSuffix(string(raw), "----\n"), "----\n") {
			creates = append(creates, strings.Split(strings.TrimSuffix(block, "\n"), "\n"))
		}
		got[in.Stage] = stageCreates{spec: spec, creates: creates}
		writeStageOutput(t, in)
		return successfulStage(in.Stage)
	})
	if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(executionStageOrder) {
		t.Fatalf("stages run: %d", len(got))
	}
	return got
}

func containsLinkHost(args []string) string {
	for _, arg := range args {
		for _, destination := range wantLinkDestinations {
			if strings.Contains(arg, strings.TrimSuffix(destination, ":443")) {
				return arg
			}
		}
	}
	return ""
}

func TestTaskLinkHostsAllowedOnlyInPrepare(t *testing.T) {
	yes := true
	got := runStagesThroughContainers(t, &contract.Network{Allow: []string{"github.com:443"}, TaskLinks: &yes}, linkTitle, linkBody, linkCriteria)
	prepare := got[model.StagePrepare]
	want := append([]string{"github.com:443"}, wantLinkDestinations...)
	if prepare.spec.Network != sandbox.NetworkDeclared || !reflect.DeepEqual(prepare.spec.AllowedDestinations, want) {
		t.Fatalf("prepare network=%s %v; want %v", prepare.spec.Network, prepare.spec.AllowedDestinations, want)
	}
	if destinations, ok := egressDestinations(prepare.creates); !ok || !reflect.DeepEqual(destinations, want) {
		t.Fatalf("prepare proxy destinations=%v; want %v", destinations, want)
	}
	for _, stage := range []model.Stage{model.StageWork, model.StageCheck, model.StagePublish} {
		for _, args := range got[stage].creates {
			if arg := containsLinkHost(args); arg != "" {
				t.Fatalf("%s create has task link host %q: %v", stage, arg, args)
			}
		}
		if arg := containsLinkHost(got[stage].spec.AllowedDestinations); arg != "" {
			t.Fatalf("%s allows task link host %q", stage, arg)
		}
	}
	if destinations, ok := egressDestinations(got[model.StagePublish].creates); !ok || !reflect.DeepEqual(destinations, []string{"api.github.com:443"}) {
		t.Fatalf("publish proxy destinations=%v", destinations)
	}
	for _, stage := range []model.Stage{model.StageWork, model.StageCheck} {
		if _, ok := egressDestinations(got[stage].creates); ok {
			t.Fatalf("%s started a proxy", stage)
		}
	}
}

func TestTaskLinksAddNoConnectionWithoutLinksOrDeclaration(t *testing.T) {
	yes := true
	t.Run("declared without links", func(t *testing.T) {
		got := runStagesThroughContainers(t, &contract.Network{TaskLinks: &yes}, "no links", "none here", "")
		prepare := got[model.StagePrepare]
		if len(prepare.spec.AllowedDestinations) != 0 {
			t.Fatalf("prepare allows %v", prepare.spec.AllowedDestinations)
		}
		workload := prepare.creates[len(prepare.creates)-1] // after the channel relay
		if _, ok := egressDestinations(prepare.creates); ok || slices.Contains(workload, "/evaloom-proxy") ||
			workload[slices.Index(workload, "--network")+1] != "none" {
			t.Fatalf("prepare is not offline: %v", prepare.creates)
		}
	})
	t.Run("not declared", func(t *testing.T) {
		got := runStagesThroughContainers(t, &contract.Network{Allow: []string{"github.com:443"}}, linkTitle, linkBody, linkCriteria)
		prepare := got[model.StagePrepare]
		if !reflect.DeepEqual(prepare.spec.AllowedDestinations, []string{"github.com:443"}) {
			t.Fatalf("prepare allows %v", prepare.spec.AllowedDestinations)
		}
		if destinations, ok := egressDestinations(prepare.creates); !ok || !reflect.DeepEqual(destinations, []string{"github.com:443"}) {
			t.Fatalf("prepare proxy destinations=%v", destinations)
		}
	})
}

// Over 50 links: no container, failed(config_error), and the reason in the
// stage log (stream=runner) and executions.error_message, through the real
// Control Plane.
func TestTooManyTaskLinksFailPrepareWithoutContainer(t *testing.T) {
	f := newLogControlPlane(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for i := range 51 {
		fmt.Fprintf(&body, "https://l%d.example.com/page\n", i)
	}
	yes := true
	_, claim, _, _ := taskLinksFixture(t, &contract.Network{TaskLinks: &yes}, "title", body.String(), "")
	claim.Execution.ID, claim.Execution.TaskID, claim.Task.ID = logExecution, "task-001", "task-001"
	// This Runner's own data and workspace roots, so prepare really reaches
	// its executor (and frozen Profile directories are cleaned up).
	r.dataDir = channelDirectory(t)
	claim.Workspace.Path = filepath.Join(r.workspaceRoot, claim.Workspace.ID)
	if err := os.MkdirAll(claim.Workspace.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE harness_profiles SET definition=? WHERE id='fixture-profile'`, string(claim.Profile.Definition)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET status='claimed',profile_id='fixture-profile',profile_commit_sha=? WHERE id=?`, claim.Profile.CommitSHA, logExecution); err != nil {
		t.Fatal(err)
	}
	config := f.config
	state := t.TempDir()
	config.Container.RuntimePath = filepath.Join(state, "runtime")
	config.Container.RuntimeEnv = map[string]string{"LINKS_STATE": state}
	if err := os.WriteFile(config.Container.RuntimePath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$LINKS_STATE/calls\"\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	err = r.ExecuteClaim(t.Context(), config, claim, func(c WorkAdapterConfig) *adapter.Registry {
		adapters := &adapter.Registry{}
		if err := adapters.Register("codex", codex.New(codex.Config{Launch: c.Launch, ResolveConfig: c.ResolveConfig,
			ResolveAuthentication: c.ResolveAuthentication, ResolveToolPermissions: c.ResolveToolPermissions, CancelGrace: c.CancelGrace})); err != nil {
			t.Fatal(err)
		}
		return adapters
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls, err := os.ReadFile(filepath.Join(state, "calls")); err == nil {
		t.Fatalf("container runtime was called: %s", calls)
	}
	var status, errorClass, message string
	if err := f.db.QueryRow(`SELECT status,error_class,coalesce(error_message,'') FROM executions WHERE id=?`, logExecution).Scan(&status, &errorClass, &message); err != nil {
		t.Fatal(err)
	}
	const reason = "config_error: タスクのリンクが 50 件を超えている"
	if status != "failed" || errorClass != "config_error" || message != strings.TrimPrefix(reason, "config_error: ") {
		t.Fatalf("execution=%s/%s message=%q", status, errorClass, message)
	}
	var text string
	if err := f.db.QueryRow(`SELECT json_extract(payload,'$.text') FROM execution_events
		WHERE execution_id=? AND type='log' AND json_extract(payload,'$.stream')='runner'`, logExecution).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != reason {
		t.Fatalf("stage log %q", text)
	}
	var stages int
	if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE execution_id=? AND type='stage_started'
		AND json_extract(payload,'$.stage') <> 'prepare'`, logExecution).Scan(&stages); err != nil || stages != 0 {
		t.Fatalf("later stages started: %d %v", stages, err)
	}
}
