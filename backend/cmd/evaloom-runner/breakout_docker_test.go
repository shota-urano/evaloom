//go:build security_docker

package main

import (
	"slices"
	"testing"

	st "github.com/shota-urano/evaloom/backend/internal/securitytest"
)

// Spec 14 3.3 on the real Sandbox (Mac mini). Not part of make verify. The
// prerequisites and variables are those of TestStageIsolationDocker:
//
//	cd backend
//	EVALOOM_SECURITY_TEST_IMAGE=evaloom-security-boundary:local \
//	EVALOOM_SECURITY_PROXY_IMAGE=evaloom-security-boundary:local \
//	EVALOOM_SECURITY_DECLARED=probe-destination.example:8080@<public-ip>:8080 \
//	EVALOOM_SECURITY_UNDECLARED=probe-undeclared.example:8081@<public-ip>:8081 \
//	EVALOOM_SECURITY_MODEL=probe-model.example:8082@<public-ip>:8082 \
//	go test -tags=security_docker ./cmd/evaloom-runner -run '^TestBreakoutDocker$' -count=1 -v -timeout 30m
//
// Missing prerequisites fail the test as unavailable; nothing is skipped.

// breakout runs one Execution whose check stage runs the script the work
// stage wrote, and whose prepare stage fetches it from the declared test
// destination (through the Sandbox proxy) and runs it. Every 3.3 row is
// attempted; the Control API access log and the destinations' records are
// judged along with the script's reports.
func (e *dockerIsolation) breakout(t *testing.T, loosen *st.Loosening) []st.Violation {
	t.Helper()
	var f *executionFixture
	r := newIsolationRun(t, isolationOptions{loosen: loosen, secrets: e.secrets, other: e.other,
		network: []string{e.declared.endpoint.Authority()}, modelAPI: []string{e.model.endpoint.Authority()},
		targets: func(fixture *executionFixture, other string) st.IsolationTargets {
			f = fixture
			return e.targets(fixture, other)
		},
		runtime: e.runtimeWiring(t),
		breakout: func(_ *executionFixture, script []byte) string {
			e.declared.server.SetFile("/"+st.BreakoutScriptName, script)
			return "http://" + e.declared.endpoint.Authority() + "/" + st.BreakoutScriptName
		}})
	violations, stages := r.verdict(t, func() st.TrustedRecords { return e.records(f) })
	if !slices.Equal(stages, []string{"check", "prepare", "publish", "work"}) {
		t.Fatalf("judged stages %v", stages)
	}
	if !fetchedScript(e.declared.server) {
		t.Fatalf("prepare did not fetch the script from the declared destination: %v", e.declared.server.Requests())
	}
	return violations
}

// TestBreakoutDocker: every access of the script fails in check and prepare
// and the Control API log has no request from the Sandboxes; then negative
// controls, each of which must make its row fail through the script.
func TestBreakoutDocker(t *testing.T) {
	e := newDockerIsolation(t)
	t.Run("isolated", func(t *testing.T) {
		if v := e.breakout(t, nil); len(v) != 0 {
			t.Fatalf("3.3 violated: %v", v)
		}
	})
	for _, tc := range []struct {
		name   string
		loosen st.Loosening
		row    string
	}{
		{"loopback_allowed_to_check", st.Loosening{Stage: st.IsolationCheck, Network: "host"}, st.RowControlAPI},
		{"write_key_to_prepare", st.Loosening{Stage: st.IsolationPrepare, Env: map[string]string{st.SecretEnvName(st.ProbeWriteSlot): e.secrets.Write}}, st.RowProbeWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loosen := tc.loosen
			if v := e.breakout(t, &loosen); !slices.Contains(st.Rows(v), tc.row) {
				t.Fatalf("loosened %s not detected as %s: %v", tc.loosen.Stage, tc.row, v)
			}
		})
	}
}
