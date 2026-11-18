package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/runner"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
	st "github.com/shota-urano/evaloom/backend/internal/securitytest"
)

// isolationRun is one Execution of the probe special harness through the
// production Runner command, with the stage plans of spec 14 3.2.
type isolationRun struct {
	f       *executionFixture
	d       st.IsolationDeclarations
	rules   map[string][]st.IsolationRule
	wiring  st.ArgsWiring
	runtime string // directory holding the recorded create commands
}

// isolationOptions select one run. The make verify run uses only the fake
// runtime and the key rows; the tagged real-Sandbox run sets everything.
type isolationOptions struct {
	loosen   *st.Loosening
	secrets  st.ProbeSecrets
	other    string   // value of a stored key no stage declares
	rows     []string // plan rows; nil means every row
	network  []string // declared destinations of prepare and publish
	modelAPI []string // destinations the work probe Adapter declares
	// targets returns the probe targets for the running fixture (other is
	// the other task's workspace).
	targets func(f *executionFixture, other string) st.IsolationTargets
	// runtime replaces the runtime CLI wiring and returns the directory
	// where create commands are recorded (default: the fake runtime's).
	runtime func(f *executionFixture) string
}

// The fake runtime runs each stage's command on the host, so only the rows
// decided by the create command, and the rows observable in the environment
// (keys), are evidence here. File and network attempts need the real
// Sandbox: stage_isolation_docker_test.go (tag security_docker).
var fakeObservableRows = []string{st.RowProbeRead, st.RowProbeWrite, st.RowUndeclaredSecret}

// modelProbe is the probe Adapter declaring a test model API destination,
// so Runner gives the Work Sandbox that destination (spec 09 3.3).
type modelProbe struct {
	adapter.Adapter
	hosts []string
}

func (m modelProbe) ModelAPIDestinations() []string { return slices.Clone(m.hosts) }

func newIsolationRun(t *testing.T, o isolationOptions) *isolationRun {
	t.Helper()
	declared := map[string][]contract.Secret{
		"prepare": {{Slot: st.ProbeReadSlot, Access: "read"}},
		"publish": {{Slot: st.ProbeReadSlot, Access: "read"}, {Slot: st.ProbeWriteSlot, Access: "write"}},
	}
	r := &isolationRun{d: st.IsolationDeclarations{Secrets: declared, ModelAPI: len(o.modelAPI) != 0}, rules: map[string][]st.IsolationRule{}}
	if len(o.network) != 0 {
		r.d.Network = map[string][]string{"prepare": o.network, "publish": o.network}
	}
	plans := map[string]json.RawMessage{}
	var otherTask string
	harness := fixtureHarness{instructions: "Probe work instructions. The probe Adapter never calls an AI.\n", taskBody: "isolation",
		populateFixture: func(t *testing.T, f *executionFixture, repo string) {
			otherTask = filepath.Join(f.config.WorkspaceRoot, "other-task")
			if err := os.MkdirAll(otherTask, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(otherTask, "marker"), []byte("other task"), 0600); err != nil {
				t.Fatal(err)
			}
			targets := st.IsolationTargets{Secrets: o.secrets, UndeclaredSecret: o.other}
			if o.targets != nil {
				targets = o.targets(f, otherTask)
			}
			for _, stage := range []string{st.IsolationPrepare, st.IsolationWork, st.IsolationCheck, st.IsolationPublish} {
				plans[stage], r.rules[stage] = st.IsolationPlan(stage, r.d, targets, o.rows...)
			}
			h := st.ProbeHarness{Plans: map[string]json.RawMessage{"prepare": plans["prepare"], "check": plans["check"], "publish": plans["publish"]},
				Secrets: declared, Network: r.d.Network}
			if err := st.WriteProbeHarness(repo, h); err != nil {
				t.Fatal(err)
			}
		}}
	f := newHarnessFixture(t, "probe", harness)
	r.f = f
	if err := o.secrets.Store(f.config.DataDir, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := secretstore.New(f.config.DataDir).Set("fixture", "probe_other", "read", []byte(o.other)); err != nil {
		t.Fatal(err)
	}
	r.runtime = f.config.Container.RuntimeEnv["EVALOOM_FAKE_RUNTIME"]
	if o.runtime != nil {
		r.runtime = o.runtime(f)
	}
	if o.loosen != nil {
		raw, _ := json.Marshal(o.loosen)
		f.config.Container.RuntimeEnv["EVALOOM_FAKE_LOOSEN"] = string(raw)
	}
	data, _ := json.Marshal(f.config)
	if err := os.WriteFile(f.configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(f.config.DataDir)
	r.wiring = st.ArgsWiring{Paths: runner.DefaultProposalPaths,
		Protected: map[string][]string{
			st.RowManagementData:  {filepath.Join(root, "control.db"), f.config.SecretsDir, f.config.TokenFile, f.config.MicroHarnessAuthFiles["codex"], filepath.Join(root, "control-data")},
			st.RowOwnerHome:       {"/var/run/docker.sock", filepath.Join(home, ".docker"), filepath.Join(home, ".ssh"), filepath.Join(home, "Library", "Keychains")},
			st.RowOtherWorkspaces: {otherTask},
		},
		Ancestors: map[string][]string{st.RowManagementData: {f.config.DataDir}, st.RowOwnerHome: {home}, st.RowOtherWorkspaces: {f.config.WorkspaceRoot}},
		Secrets:   append(o.secrets.Values(), o.other)}
	f.workAdapter = func(w runner.WorkAdapterConfig) adapter.Adapter {
		a, err := st.NewWorkProbe(w, plans["work"])
		if err != nil {
			t.Error(err)
		}
		if len(o.modelAPI) != 0 {
			return modelProbe{Adapter: a, hosts: o.modelAPI}
		}
		return a
	}
	return r
}

// verdict runs the Execution and returns the violations of both kinds of
// evidence, and the stages whose create commands were judged.
func (r *isolationRun) verdict(t *testing.T, records func() st.TrustedRecords) ([]st.Violation, []string) {
	t.Helper()
	f := r.f
	f.start()
	f.waitStatus("succeeded")
	trusted := st.TrustedRecords{}
	if records != nil {
		trusted = records()
	}
	f.stages([]string{"prepare-1", "work-1", "check-1", "publish-1"})
	var violations []st.Violation
	var stages []string
	created, err := filepath.Glob(filepath.Join(r.runtime, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range created {
		raw, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		var args []string
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatal(err)
		}
		parsed, err := st.ParseCreateArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		stage := st.StageOf(parsed, r.wiring.Paths)
		if stage == "" && slices.Equal(parsed.Flags["--entrypoint"], []string{"/evaloom-proxy"}) {
			continue // trusted proxy sidecars of a networked stage (spec 09 3.3)
		}
		if stage == "" {
			// Configuration-volume helpers (snapshot/copy) carry no stage
			// mounts. Accept them only offline and without host paths.
			if parsed.Network != "none" || slices.ContainsFunc(parsed.Mounts, func(m st.CreateMount) bool { return m.Type != "volume" }) || len(parsed.Imported) != 0 {
				t.Fatalf("unrecognized container %v", args)
			}
			continue
		}
		if !slices.Contains(stages, stage) {
			stages = append(stages, stage)
		}
		violations = append(violations, st.CreateArgsVerdict(stage, args, r.d, r.wiring)...)
	}
	slices.Sort(stages)
	for stage, rules := range r.rules {
		var report string
		if stage == st.IsolationWork {
			_ = filepath.WalkDir(f.config.WorkspaceRoot, func(path string, entry fs.DirEntry, err error) error {
				if err == nil && strings.HasSuffix(filepath.ToSlash(path), "/"+st.WorkProbeReport) {
					report = path
				}
				return err
			})
		} else {
			report = filepath.Join(f.config.DataDir, "executions", f.id, stage+"-1", "out", "probe-"+stage+".json")
		}
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatalf("%s report: %v", stage, err)
		}
		violations = append(violations, st.ObservationVerdict(stage, data, rules, r.d, trusted)...)
	}
	return violations, stages
}

// Spec 14 3.2 in make verify. The production Runner command runs prepare,
// work (probe Adapter), check and publish; only the container runtime CLI
// is a fake. Every stage's create command is judged against the 3.2 table
// (network, visible and writable locations, protected host paths, keys,
// environment), and the in-Sandbox key probes against their cells. Each
// negative control loosens one stage at the runtime CLI boundary and must
// make the corresponding row fail.
func TestStageIsolationThroughRunner(t *testing.T) {
	secrets := st.NewProbeSecrets()
	other := st.NewCanary()
	t.Run("isolated", func(t *testing.T) {
		violations, stages := newIsolationRun(t, isolationOptions{secrets: secrets, other: other, rows: fakeObservableRows}).verdict(t, nil)
		if len(violations) != 0 {
			t.Fatalf("3.2 violated: %v", violations)
		}
		if !slices.Equal(stages, []string{"check", "prepare", "publish", "work"}) {
			t.Fatalf("judged stages %v", stages)
		}
	})
	for _, tc := range []struct {
		name   string
		loosen st.Loosening
		row    string
	}{
		{"loopback_allowed", st.Loosening{Stage: st.IsolationCheck, Network: "host"}, st.RowControlAPI},
		{"key_to_work", st.Loosening{Stage: st.IsolationWork, Env: map[string]string{st.SecretEnvName(st.ProbeReadSlot): secrets.Read}}, st.RowProbeRead},
		{"write_key_to_prepare", st.Loosening{Stage: st.IsolationPrepare, Env: map[string]string{st.SecretEnvName(st.ProbeWriteSlot): secrets.Write}}, st.RowProbeWrite},
		{"undeclared_key_to_publish", st.Loosening{Stage: st.IsolationPublish, Env: map[string]string{"EVALOOM_SECRET_PROBE_OTHER": other}}, st.RowUndeclaredSecret},
		{"data_directory_to_check", st.Loosening{Stage: st.IsolationCheck, Mounts: []string{"type=bind,source=/,target=/host,readonly"}}, st.RowManagementData},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loosen := tc.loosen
			violations, _ := newIsolationRun(t, isolationOptions{loosen: &loosen, secrets: secrets, other: other, rows: fakeObservableRows}).verdict(t, nil)
			if !slices.Contains(st.Rows(violations), tc.row) {
				t.Fatalf("loosened %s not detected as %s: %v", tc.loosen.Stage, tc.row, violations)
			}
			for _, v := range violations {
				if v.Stage != tc.loosen.Stage {
					t.Fatalf("violation outside the loosened stage: %v", v)
				}
			}
		})
	}
}
