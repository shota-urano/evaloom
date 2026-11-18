package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/runner"
	"github.com/shota-urano/evaloom/backend/internal/securitytest"
)

// secretPlan asks, per test secret, whether its value is in the environment.
// The plan holds only hashes; the canary values never reach the report.
func secretPlan(secrets securitytest.ProbeSecrets) json.RawMessage {
	plan, _ := json.Marshal([]any{securitytest.SecretProbe("probe-read", secrets.Read), securitytest.SecretProbe("probe-write", secrets.Write)})
	return plan
}

func secretObservations(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	observations, err := probe.DecodeReport(data)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	got := map[string]bool{}
	for _, o := range observations {
		if o.Kind != "secret" {
			t.Fatalf("%s: unexpected observation %+v", path, o)
		}
		got[o.ID] = *o.Succeeded
	}
	if len(got) != 2 {
		t.Fatalf("%s: observations missing %v", path, got)
	}
	return got
}

// Spec 14 7j6.1: the probe special harness, the probe Adapter and the test
// secrets carry an Execution from prepare to publish to succeeded through the
// production Runner command. Only the container runtime CLI is the test
// runtime (no Docker daemon); Sandbox isolation itself is proven by the
// tagged real-environment tests, which require the prerequisites.
func TestProbeHarnessExecutionSucceeds(t *testing.T) {
	// Plans committed with the harness hold only the canaries' hashes; the
	// values go to the Runner's secret store below.
	secrets := securitytest.NewProbeSecrets()
	plan := secretPlan(secrets)
	harness := fixtureHarness{instructions: "Probe work instructions. The probe Adapter never calls an AI.\n", taskBody: "probe execution",
		populate: func(t *testing.T, repo, _ string) {
			h := securitytest.ProbeHarness{
				Plans: map[string]json.RawMessage{"prepare": plan, "check": plan, "publish": plan},
				Secrets: map[string][]contract.Secret{
					"prepare": {{Slot: securitytest.ProbeReadSlot, Access: "read"}},
					"publish": {{Slot: securitytest.ProbeReadSlot, Access: "read"}, {Slot: securitytest.ProbeWriteSlot, Access: "write"}},
				},
			}
			if err := securitytest.WriteProbeHarness(repo, h); err != nil {
				t.Fatal(err)
			}
		}}
	f := newHarnessFixture(t, "probe", harness)
	if err := secrets.Store(f.config.DataDir, "fixture"); err != nil {
		t.Fatal(err)
	}
	f.workAdapter = func(w runner.WorkAdapterConfig) adapter.Adapter {
		a, err := securitytest.NewWorkProbe(w, plan)
		if err != nil {
			t.Error(err)
		}
		return a
	}
	f.start()
	f.waitStatus("succeeded")
	f.stages([]string{"prepare-1", "work-1", "check-1", "publish-1"})

	stage := func(name string) string {
		return filepath.Join(f.config.DataDir, "executions", f.id, name+"-1", "out", "probe-"+name+".json")
	}
	want := map[string]map[string]bool{
		"prepare": {"probe-read": true, "probe-write": false},
		"check":   {"probe-read": false, "probe-write": false},
		"publish": {"probe-read": true, "probe-write": true},
	}
	for name, expected := range want {
		if got := secretObservations(t, stage(name)); fmt.Sprint(got) != fmt.Sprint(expected) {
			t.Fatalf("%s secrets %v; want %v", name, got, expected)
		}
	}
	var workReports []string
	if err := filepath.WalkDir(f.config.WorkspaceRoot, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && filepath.ToSlash(path[len(filepath.Dir(filepath.Dir(path)))+1:]) == securitytest.WorkProbeReport {
			workReports = append(workReports, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(workReports) != 1 {
		t.Fatalf("work probe reports %v", workReports)
	}
	if got := secretObservations(t, workReports[0]); got["probe-read"] || got["probe-write"] {
		t.Fatalf("work stage saw test secrets: %v", got)
	}
	// Reports and records hold only booleans: no canary value is retained.
	for _, root := range []string{filepath.Join(f.config.DataDir, "executions"), f.config.WorkspaceRoot} {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !entry.Type().IsRegular() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, value := range secrets.Values() {
				if bytes.Contains(data, []byte(value)) {
					return fmt.Errorf("canary retained in %s", path)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
