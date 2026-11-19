//go:build security_docker

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	st "github.com/shota-urano/evaloom/backend/internal/securitytest"
)

// Spec 14 3.6, hostile Issue: use ExecuteProposal (including its claim input
// and result report), the real Container, every row of the Proposal column
// of 3.2, and independent Control Plane/destination records. Prerequisites
// match TestStageIsolationDocker; absence is a failure, never a skip.
func TestProposalIsolationDocker(t *testing.T) {
	for _, tc := range []struct{ name, row string }{
		{"isolated", ""},
		{"negative_control_key_to_proposal", st.RowProbeRead},
		{"negative_control_host_network", st.RowControlAPI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDockerIsolation(t)
			p := newProposalFixture(t)
			otherTask := filepath.Join(p.config.WorkspaceRoot, "other-task")
			if err := os.MkdirAll(otherTask, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(otherTask, "marker"), []byte("other task"), 0600); err != nil {
				t.Fatal(err)
			}
			targets := e.targets(p.executionFixture, otherTask)
			targets.Secrets, targets.UndeclaredSecret = p.secrets, p.otherSecret
			plan, rules := st.IsolationPlan(st.IsolationProposal, st.IsolationDeclarations{ModelAPI: true}, targets)
			probe := &st.ProposalProbe{Plan: plan, Label: "route:fixture", Reason: "probe selection", Destinations: []string{e.model.endpoint.Authority()}}
			recordsDir := e.runtimeWiring(t)(p.executionFixture)
			if tc.row != "" {
				loosen := st.Loosening{Stage: st.IsolationProposal, Network: "host"}
				if tc.row == st.RowProbeRead {
					loosen.Network = ""
					loosen.Env = map[string]string{st.SecretEnvName(st.ProbeReadSlot): p.secrets.Read}
				}
				raw, _ := json.Marshal(loosen)
				p.config.Container.RuntimeEnv["EVALOOM_FAKE_LOOSEN"] = string(raw)
			}
			p.run(p.claim(), probe)
			v := p.isolationVerdict(probe, rules, e.records(p.executionFixture), recordsDir)
			if tc.row == "" && len(v) != 0 {
				t.Fatalf("3.6 hostile Issue violated Proposal isolation: %v", v)
			}
			if tc.row != "" && !slices.Contains(st.Rows(v), tc.row) {
				t.Fatalf("weakened Proposal isolation not detected as %s: %v", tc.row, v)
			}
			if tc.row == st.RowProbeRead {
				observed := st.ObservationVerdict(st.IsolationProposal, probe.Report, rules, st.IsolationDeclarations{ModelAPI: true}, st.TrustedRecords{})
				if !slices.Contains(st.Rows(observed), st.RowProbeRead) {
					t.Fatal("create arguments changed, but the probe did not observe the leaked key")
				}
			}
			p.wantWaiting("proposed", "")
		})
	}
}
