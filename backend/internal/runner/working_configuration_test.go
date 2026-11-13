package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/claudecode"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Keep both real Adapters, the work executor, output validation, session
// retention, HTTP reporting and SQLite. Only the external container runtime is
// deterministic; it updates the private volume rather than host staging files.
func TestWorkingConfigurationStageLifecycle(t *testing.T) {
	for _, harness := range []string{"codex", "claude_code"} {
		for _, mode := range []string{"success", "failure", "question", "invalid-question", "cancel", "mismatch"} {
			t.Run(harness+"/"+mode, func(t *testing.T) {
				f := newLogControlPlane(t)
				r, claim, _, _ := stageFixture(t, false)
				process, err := New(f.config)
				if err != nil {
					t.Fatal(err)
				}
				r.controlPlaneURL, r.client, r.token = process.controlPlaneURL, process.client, process.token
				claim.Execution.ID, claim.Task.ID = logExecution, "task-001"
				claim.Profile.HarnessID = "fixture-harness"
				var definition contract.ProfileDefinition
				if err := json.Unmarshal(claim.Profile.Definition, &definition); err != nil {
					t.Fatal(err)
				}
				definition.Stages.Work.MicroHarness = harness
				claim.Profile.Definition, err = json.Marshal(definition)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE harness_profiles SET definition=? WHERE id='fixture-profile'`, string(claim.Profile.Definition)); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE executions SET status='claimed',profile_id='fixture-profile',profile_commit_sha=?,harness_id='fixture-harness' WHERE id=?`, claim.Profile.CommitSHA, logExecution); err != nil {
					t.Fatal(err)
				}
				state := t.TempDir()
				authName, variable := "auth.json", "CODEX_HOME"
				if harness == "claude_code" {
					authName, variable = ".credentials.json", "CLAUDE_CONFIG_DIR"
				}
				store := filepath.Join(r.dataDir, authName)
				if err := os.WriteFile(store, []byte(`{"token":"original-token"}`), 0600); err != nil {
					t.Fatal(err)
				}
				config := f.config
				config.MicroHarnessAuthFiles = map[string]string{harness: store}
				config.Container.RuntimePath = filepath.Join(state, "runtime")
				config.Container.ProxyBinary = "/bin/sh"
				config.Container.RuntimeEnv = map[string]string{"COPY_STATE": state, "COPY_MODE": mode,
					"COPY_HARNESS": harness, "COPY_AUTH": authName, "COPY_VARIABLE": variable}
				if err := os.WriteFile(config.Container.RuntimePath, []byte(workingCopyRuntime), 0700); err != nil {
					t.Fatal(err)
				}
				e := &executionExecutor{runner: r, claim: claim, config: config, paths: DefaultProposalPaths, adapters: &adapter.Registry{}}
				w := e.workAdapterConfig()
				var selected adapter.Adapter
				if harness == "codex" {
					selected = codex.New(codex.Config{Launch: w.Launch, ResolveConfig: w.ResolveConfig,
						ResolveAuthentication: w.ResolveAuthentication, ResolveToolPermissions: w.ResolveToolPermissions, CancelGrace: w.CancelGrace})
				} else {
					selected = claudecode.New(claudecode.Config{Launch: w.Launch, ResolveConfig: w.ResolveConfig,
						ResolveAuthentication: w.ResolveAuthentication, ResolveToolPermissions: w.ResolveToolPermissions, CancelGrace: w.CancelGrace})
				}
				var next StageNext
				var directory string
				err = r.WithProfile(t.Context(), claim, func(ctx context.Context, profile LoadedProfile) error {
					request, err := stageRequest(claim, model.StageWork, e.paths, stageResume{})
					if err != nil {
						return err
					}
					attempt, err := r.StartStage(ctx, logExecution, model.StageWork, request)
					if err != nil {
						return err
					}
					directory, err = r.prepareStageFiles(logExecution, model.StageWork, attempt, request, nil)
					if err != nil {
						return err
					}
					config.Container.RuntimeEnv["COPY_OUT"] = filepath.Join(directory, "out")
					ctx, cancel := context.WithCancel(ctx)
					defer cancel()
					if mode == "cancel" {
						go func() {
							ticker := time.NewTicker(time.Millisecond)
							defer ticker.Stop()
							for {
								select {
								case <-ctx.Done():
									return
								case <-ticker.C:
									if _, err := os.Stat(filepath.Join(state, "running")); err == nil {
										cancel()
										return
									}
								}
							}
						}()
					}
					in := StageInvocation{Stage: model.StageWork, Attempt: attempt, Profile: profile, Workspace: claim.Workspace.Path,
						Directory: directory, Adapter: selected, timeout: time.Minute}
					outcome := r.runStageWithSecrets(ctx, logExecution, claim.Profile.HarnessID, in, e)
					next, err = r.FinishStage(context.WithoutCancel(ctx), logExecution, model.StageWork, attempt, outcome)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				wantRun, wantError, wantNext := "failed", "work_failed", "stop"
				switch mode {
				case "success":
					wantRun, wantError, wantNext = "succeeded", "", "check"
				case "question":
					wantRun, wantError = "question", ""
				case "cancel":
					wantRun, wantError = "cancelled", "cancelled"
				case "mismatch":
					wantError = "config_error"
				}
				var run, class string
				if err := f.db.QueryRow(`SELECT status,coalesce(error_class,'') FROM stage_runs WHERE execution_id=? AND stage='work'`, logExecution).Scan(&run, &class); err != nil {
					t.Fatal(err)
				}
				if run != wantRun || class != wantError || next.Next != wantNext {
					t.Fatalf("stage=%s/%s next=%s; want=%s/%s next=%s", run, class, next.Next, wantRun, wantError, wantNext)
				}
				entries, err := os.ReadDir(filepath.Join(directory, "configuration"))
				if err != nil || len(entries) != 0 {
					t.Fatalf("stage configuration survived: %v %v", entries, err)
				}
				copyDir, err := os.ReadFile(filepath.Join(state, "copy-dir"))
				if err != nil || !strings.HasPrefix(string(copyDir), filepath.Join(directory, "configuration", "harness-")) {
					t.Fatalf("CLI did not receive a stage-local %s: %q %v", variable, copyDir, err)
				}
				if _, err := os.Stat(filepath.Join(state, "active-volume")); !os.IsNotExist(err) {
					t.Fatalf("working volume survived stage: %v", err)
				}
				retained := filepath.Join(r.dataDir, "work-sessions", claim.Workspace.ID, logExecution, "current", "configuration")
				if mode == "question" {
					info, err := os.Stat(retained)
					if err != nil || info.Mode().Perm() != 0700 {
						t.Fatalf("retained permissions: %v %v", info, err)
					}
					for name, want := range map[string]string{"session-state": "CLI state", "work/instructions.md": "committed instructions\n"} {
						data, err := os.ReadFile(filepath.Join(retained, name))
						if err != nil || string(data) != want {
							t.Fatalf("retained %s=%q %v; want=%q", name, data, err, want)
						}
					}
					for _, name := range []string{"auth.json", ".credentials.json"} {
						if _, err := os.Lstat(filepath.Join(retained, name)); !os.IsNotExist(err) {
							t.Fatalf("retained authentication: %s %v", name, err)
						}
					}
				} else if _, err := os.Stat(retained); !os.IsNotExist(err) {
					t.Fatalf("non-question configuration retained: %v", err)
				}
				data, err := os.ReadFile(store)
				wantToken := "refreshed-token"
				if mode == "mismatch" {
					wantToken = "original-token"
					if _, err := os.Stat(filepath.Join(state, "cli-started")); !os.IsNotExist(err) {
						t.Fatal("CLI launched with mismatched configuration")
					}
				}
				if err != nil || !strings.Contains(string(data), wantToken) {
					t.Fatalf("authentication was not recovered: %v", err)
				}
				var runtimeRevisions int
				if err := f.db.QueryRow(`SELECT count(*) FROM context_revisions WHERE execution_id=? AND (source_path IN ('auth.json','.credentials.json','session-state'))`, logExecution).Scan(&runtimeRevisions); err != nil || runtimeRevisions != 0 {
					t.Fatalf("CLI state entered context revisions: %d %v", runtimeRevisions, err)
				}
			})
		}
	}
}

const workingCopyRuntime = `#!/bin/sh
set -eu
export COPYFILE_DISABLE=1
for arg in "$@"; do last="$arg"; done
case "$1 $2" in
 'image inspect') printf '%s' '{"Os":"linux","Config":{}}' ;;
 'volume create') case "$last" in evaloom-config-*) : > "$COPY_STATE/active-volume" ;; esac ;;
 'volume rm') case "$last" in evaloom-config-*) /bin/rm -f "$COPY_STATE/active-volume" ;; esac ;;
 'container create')
  case "$*" in *evaloom-proxy*) exit 0 ;; esac
  phase=exec
  for arg in "$@"; do
   case "$arg" in
    evaloom-codex-preflight|evaloom-claude-preflight) phase=preflight ;;
   esac
   case "$arg" in
    "$COPY_VARIABLE"=*) printf '%s' "${arg#*=}" > "$COPY_STATE/copy-dir" ;;
   esac
  done
  printf '%s' "$phase" > "$COPY_STATE/phase" ;;
 'container cp')
  if [ "$3" = '-' ]; then
   /bin/cat > "$COPY_STATE/configuration.tar"
   /bin/mkdir -p "$COPY_STATE/volume"
   /usr/bin/tar -xf "$COPY_STATE/configuration.tar" -C "$COPY_STATE/volume"
   if [ "$COPY_MODE" = mismatch ]; then
    printf changed > "$COPY_STATE/volume/work/instructions.md"
   fi
  else
   case "$3" in
    *:/evaloom-auth/*) /usr/bin/tar -cf - -C "$COPY_STATE/volume" "$COPY_AUTH" ;;
    *) /usr/bin/tar -cf - -C "$COPY_STATE/volume" . ;;
   esac
  fi ;;
 'container start')
  case "$last" in *-ingress|*-egress) exit 0 ;; esac
  : > "$COPY_STATE/cli-started"
  if [ "$(/bin/cat "$COPY_STATE/phase")" = preflight ]; then exit 0; fi
  printf 'CLI state' > "$COPY_STATE/volume/session-state"
  printf '%s' '{"token":"refreshed-token"}' > "$COPY_STATE/volume/$COPY_AUTH"
  case "$COPY_MODE" in
   question|failure|cancel) printf '%s' '{"question":"Continue?"}' > "$COPY_OUT/question.json" ;;
   invalid-question) printf '{' > "$COPY_OUT/question.json" ;;
  esac
  if [ "$COPY_MODE" = cancel ]; then
   : > "$COPY_STATE/running"
   while [ ! -f "$COPY_STATE/stopped" ]; do /bin/sleep 0.01; done
   exit 0
  fi
  if [ "$COPY_HARNESS" = codex ]; then
   printf '%s\n' '{"type":"thread.started","thread_id":"native-session"}' '{"type":"turn.completed"}'
  else
   printf '%s\n' '{"type":"result","session_id":"native-session","subtype":"success","is_error":false,"result":"done"}'
  fi ;;
 'container inspect')
  case "$last" in
   *-ingress) printf '%s' '{"State":{"Running":true},"HostConfig":{"NetworkMode":"none"},"NetworkSettings":{"Networks":{"none":{}}}}'; exit 0 ;;
   *-egress) printf '%s' '{"State":{"Running":true},"HostConfig":{"NetworkMode":"bridge"},"NetworkSettings":{"Networks":{"bridge":{}}}}'; exit 0 ;;
  esac
  code=0
  if [ "$COPY_MODE" = failure ] && [ "$(/bin/cat "$COPY_STATE/phase")" = exec ]; then code=23; fi
  printf '{"Status":"exited","Running":false,"ExitCode":%s}' "$code" ;;
 'container stop'|'container kill') : > "$COPY_STATE/stopped" ;;
 'container rm'|'container exec') ;;
 *) exit 99 ;;
esac
`
