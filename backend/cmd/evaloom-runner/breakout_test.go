package main

import (
	"net"
	"net/http"
	"slices"
	"testing"

	st "github.com/shota-urano/evaloom/backend/internal/securitytest"
)

// breakoutDestination serves the breakout script for the prepare stage and
// records every request it receives.
func breakoutDestination(t *testing.T, listen string) *st.Destination {
	t.Helper()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatalf("実行不能（スキップではない）: %v", err)
	}
	d := st.NewDestination(listener)
	t.Cleanup(func() { d.Close() })
	return d
}

func fetchedScript(d *st.Destination) bool {
	return slices.ContainsFunc(d.Requests(), func(r st.DestinationRequest) bool {
		return r.Method == http.MethodGet && r.Path == "/"+st.BreakoutScriptName
	})
}

// Spec 14 3.3 in make verify. The production Runner command runs the probe
// special harness: the work stage (probe Adapter) writes the breakout
// script into the workspace, the check stage runs it from the workspace,
// and the prepare stage fetches it from the test destination and runs it.
// Only the container runtime CLI is a fake, which runs each command on the
// host, so the evidence here is what does not depend on a real Sandbox:
// every stage's create command (CreateArgsVerdict), the keys the script
// finds in its environment, and authorize on the run channel, which Runner
// must refuse with no request reaching the Control API access log. File and
// network attempts need the real Sandbox: breakout_docker_test.go (tag
// security_docker).
func TestBreakoutThroughRunner(t *testing.T) {
	secrets := st.NewProbeSecrets()
	other := st.NewCanary()
	rows := append(slices.Clone(fakeObservableRows), st.RowAuthorize)
	run := func(t *testing.T, loosen *st.Loosening) ([]st.Violation, *st.Destination) {
		t.Helper()
		// The fake runtime has no proxy, so prepare declares no network and
		// fetches directly; only the real run uses a declared destination.
		d := breakoutDestination(t, "127.0.0.1:0")
		r := newIsolationRun(t, isolationOptions{loosen: loosen, secrets: secrets, other: other, rows: rows,
			breakout: func(_ *executionFixture, script []byte) string {
				d.SetFile("/"+st.BreakoutScriptName, script)
				return d.URL("/" + st.BreakoutScriptName)
			}})
		violations, stages := r.verdict(t, func() st.TrustedRecords {
			r.f.mu.Lock()
			defer r.f.mu.Unlock()
			return st.TrustedRecords{ControlAPI: slices.Clone(r.f.accesses)}
		})
		if !slices.Equal(stages, []string{"check", "prepare", "publish", "work"}) {
			t.Fatalf("judged stages %v", stages)
		}
		return violations, d
	}
	t.Run("isolated", func(t *testing.T) {
		violations, d := run(t, nil)
		if len(violations) != 0 {
			t.Fatalf("3.3 violated: %v", violations)
		}
		if !fetchedScript(d) {
			t.Fatalf("prepare did not fetch the script: %v", d.Requests())
		}
	})
	for _, tc := range []struct {
		name   string
		loosen st.Loosening
		row    string
	}{
		{"write_key_to_check", st.Loosening{Stage: st.IsolationCheck, Env: map[string]string{st.SecretEnvName(st.ProbeWriteSlot): secrets.Write}}, st.RowProbeWrite},
		{"read_key_to_check", st.Loosening{Stage: st.IsolationCheck, Env: map[string]string{st.SecretEnvName(st.ProbeReadSlot): secrets.Read}}, st.RowProbeRead},
		{"write_key_to_prepare", st.Loosening{Stage: st.IsolationPrepare, Env: map[string]string{st.SecretEnvName(st.ProbeWriteSlot): secrets.Write}}, st.RowProbeWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loosen := tc.loosen
			violations, _ := run(t, &loosen)
			found := false
			for _, v := range violations {
				if v.Stage != tc.loosen.Stage {
					t.Fatalf("violation outside the loosened stage: %v", v)
				}
				// The script itself found the key (not only the create command).
				found = found || (v.Row == tc.row && v.Detail == tc.row+"/value succeeded")
			}
			if !found {
				t.Fatalf("loosened %s not detected by the breakout script as %s: %v", tc.loosen.Stage, tc.row, violations)
			}
		})
	}
}
