package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

func TestCodexWorkReportsOnlyNativeSession(t *testing.T) {
	for _, mode := range []string{"success", "optional_acceptance", "work_failure", "config_failure"} {
		t.Run(mode, func(t *testing.T) {
			p, f := newProfileFixture(t), newLogControlPlane(t)
			r, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			claim := p.claim
			claim.Execution.ID, claim.Task.AcceptanceCriteria = logExecution, "work completes"
			if mode == "optional_acceptance" {
				claim.Task.AcceptanceCriteria = ""
			}
			if _, err := f.db.Exec(`UPDATE harness_profiles SET definition=? WHERE id='fixture-profile'`, string(claim.Profile.Definition)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE executions SET profile_id='fixture-profile',profile_commit_sha=?,harness_id='fixture-harness' WHERE id=?`, claim.Profile.CommitSHA, logExecution); err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(store, []byte(`{"token":"fixture-token"}`), 0600); err != nil {
				t.Fatal(err)
			}
			e := &executionExecutor{runner: r, claim: claim, config: f.config, paths: DefaultProposalPaths}
			e.config.MicroHarnessAuthFiles = map[string]string{"codex": store}
			in := StageInvocation{Stage: model.StageWork, Profile: LoadedProfile{Definition: p.def}, Directory: t.TempDir(), timeout: time.Minute}
			if err := os.WriteFile(filepath.Join(in.Directory, "request.json"), []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			w := e.workAdapterConfig()
			calls := 0
			in.Adapter = codex.New(codex.Config{ResolveConfig: w.ResolveConfig, ResolveAuthentication: w.ResolveAuthentication,
				ResolveToolPermissions: w.ResolveToolPermissions, CancelGrace: w.CancelGrace,
				// Replace only the external command boundary. Keep Codex output
				// parsing, authentication, runWork and context API calls real.
				Launch: func(_ context.Context, _ adapter.SandboxEnv, _ []string, stdout, _ io.Writer) (sandbox.Process, error) {
					calls++
					if mode == "config_failure" {
						return nil, &harnesscontext.ConfigError{Err: os.ErrNotExist}
					}
					if calls == 2 {
						_, _ = io.WriteString(stdout, "{\"type\":\"thread.started\",\"thread_id\":\"cli-native-session\"}\n")
						if mode == "work_failure" {
							_, _ = io.WriteString(stdout, "{\"type\":\"turn.failed\"}\n")
						} else {
							_, _ = io.WriteString(stdout, "{\"type\":\"turn.completed\"}\n")
						}
					}
					return instantProposalProcess{}, nil
				},
			})
			log, err := r.NewLog(logExecution, "work", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer log.Finish(false)
			out := e.runWork(t.Context(), in, sandbox.SandboxSpec{}, sandbox.ContainerConfig{}, log)
			if mode == "config_failure" {
				if out.Work != nil || out.RunnerError == nil || *out.RunnerError != "config_error" {
					t.Fatalf("pre-session failure invented a native session: %+v", out)
				}
				return
			}
			if out.Work == nil || out.Work.SessionID == nil || *out.Work.SessionID != "cli-native-session" {
				t.Fatalf("native session lost: %+v", out)
			}
			if mode == "work_failure" {
				if out.RunnerError == nil || *out.RunnerError != "work_failed" {
					t.Fatalf("work failure misclassified: %+v", out)
				}
			} else if out.RunnerError != nil || out.ExitCode == nil || *out.ExitCode != 0 {
				t.Fatalf("success changed: %+v", out)
			}
		})
	}
}
