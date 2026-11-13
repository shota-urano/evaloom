package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/dummy"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// This Adapter substitutes only the micro-harness. Alias results keep the
// original CLI session identifier while each claim has a fresh Adapter object.
type questionSessionAdapter struct {
	adapter.Adapter
	resume   bool
	alias    map[adapter.SessionID]adapter.SessionID
	onResume func(adapter.SessionID)
}

func (a *questionSessionAdapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Resume: a.resume}
}
func (a *questionSessionAdapter) Resume(ctx context.Context, id adapter.SessionID, req adapter.HarnessRequest, env adapter.SandboxEnv) error {
	a.onResume(id)
	next, err := a.Adapter.Start(ctx, req, env)
	if err == nil {
		a.alias[id] = next
	}
	return err
}
func (a *questionSessionAdapter) resolve(id adapter.SessionID) adapter.SessionID {
	if next, ok := a.alias[id]; ok {
		return next
	}
	return id
}
func (a *questionSessionAdapter) Status(id adapter.SessionID) (adapter.SessionStatus, error) {
	return a.Adapter.Status(a.resolve(id))
}
func (a *questionSessionAdapter) Logs(id adapter.SessionID) (<-chan adapter.LogLine, error) {
	return a.Adapter.Logs(a.resolve(id))
}
func (a *questionSessionAdapter) Result(id adapter.SessionID) (adapter.HarnessResult, error) {
	r, err := a.Adapter.Result(a.resolve(id))
	r.SessionID = id
	return r, err
}

// Keep Profile loading, stage execution, Sandbox creation/supervision, output
// validation, authenticated reporting, owner answers, claims and SQLite real.
// Only the micro-harness and container runtime are deterministic substitutes.
func TestQuestionSessionContinuation(t *testing.T) {
	for _, scenario := range []string{"resume", "missing", "missing-file", "unsupported", "no-id", "required", "required-no-id", "tampered", "workspace-missing", "repeat"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLogControlPlane(t)
			r, claim, _, _ := stageFixture(t, false)
			process, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			r.controlPlaneURL, r.client, r.token, r.claimURL = process.controlPlaneURL, process.client, process.token, process.claimURL
			claim.Execution.ID, claim.Execution.TaskID, claim.Task.ID = logExecution, "task-001", "task-001"
			claim.Execution.Seq = 1
			claim.Profile.ID, claim.Profile.HarnessID = "fixture-profile", "fixture-harness"
			claim.Route = json.RawMessage(`{"kind":"project","projectId":"project","projectName":"Project"}`)
			claim.Params = json.RawMessage(`{}`)
			var definition contract.ProfileDefinition
			if err := json.Unmarshal(claim.Profile.Definition, &definition); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario, "required") {
				definition.Stages.Work.Resume = "required"
			}
			claim.Profile.Definition, _ = json.Marshal(definition)
			for _, statement := range []struct {
				query string
				args  []any
			}{
				{`UPDATE harnesses SET repo_path=? WHERE id='fixture-harness'`, []any{claim.Profile.RepoPath}},
				{`UPDATE harness_profiles SET definition=?,commit_sha=? WHERE id='fixture-profile'`, []any{string(claim.Profile.Definition), claim.Profile.CommitSHA}},
				{`INSERT INTO task_workspaces(id,task_id,runner_id,path,status,created_by_execution_id,created_at) VALUES (?,'task-001','mac-mini',?,'active',?,'now')`, []any{claim.Workspace.ID, claim.Workspace.Path, logExecution}},
				{`UPDATE executions SET status='claimed',profile_id='fixture-profile',profile_version='1.0.0',profile_commit_sha=?,harness_id='fixture-harness',route_kind='project',project_id='project',workspace_id=?,params='{}',contract_version='1' WHERE id=?`, []any{claim.Profile.CommitSHA, claim.Workspace.ID, logExecution}},
			} {
				if _, err := f.db.Exec(statement.query, statement.args...); err != nil {
					t.Fatal(err)
				}
			}
			config := f.config
			config.DataDir, config.WorkspaceRoot = r.dataDir, r.workspaceRoot
			auth := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(auth, []byte(`{"token":"before-answer"}`), 0600); err != nil {
				t.Fatal(err)
			}
			config.MicroHarnessAuthFiles = map[string]string{"codex": auth}
			runtimeRoot := t.TempDir()
			config.Container.RuntimePath = filepath.Join(runtimeRoot, "runtime")
			config.Container.RuntimeEnv = map[string]string{"SESSION_RUNTIME_ROOT": runtimeRoot}
			if err := os.WriteFile(config.Container.RuntimePath, []byte(`#!/bin/sh
case "$1 $2" in
 'image inspect') printf '%s' '{"Os":"linux","Config":{}}' ;;
 'volume create'|'volume rm'|'container create'|'container rm') ;;
 'container cp')
  if [ "$3" = '-' ]; then
   /bin/cat > "$SESSION_RUNTIME_ROOT/configuration.tar"
  else
   case "$3" in
    *:/evaloom-session/.) /bin/cat "$SESSION_RUNTIME_ROOT/session.tar" ;;
    *) /bin/cat "$SESSION_RUNTIME_ROOT/configuration.tar" ;;
   esac
  fi ;;
 'container inspect') printf '%s' '{"Status":"exited","Running":false,"ExitCode":0}' ;;
 'container start')
  for arg in "$@"; do last="$arg"; done
  printf '%s\n' "$last" >> "$SESSION_RUNTIME_ROOT/launches"
  /bin/mkdir -p "$SESSION_RUNTIME_ROOT/volume"
  /usr/bin/tar -xf "$SESSION_RUNTIME_ROOT/configuration.tar" -C "$SESSION_RUNTIME_ROOT/volume"
  count=$(/usr/bin/wc -l < "$SESSION_RUNTIME_ROOT/launches")
  printf '%s' "$count" | /usr/bin/tr -d ' ' > "$SESSION_RUNTIME_ROOT/volume/session-state"
  /usr/bin/tar -cf "$SESSION_RUNTIME_ROOT/session.tar" -C "$SESSION_RUNTIME_ROOT/volume" . ;;
 *) exit 99 ;;
esac
`), 0700); err != nil {
				t.Fatal(err)
			}
			calls, resumes := 0, 0
			var firstSession string
			var workContexts []adapter.RequestContext
			var configDirs []string
			factory := func(c WorkAdapterConfig) *adapter.Registry {
				selected := &questionSessionAdapter{resume: scenario != "unsupported", alias: map[adapter.SessionID]adapter.SessionID{}, onResume: func(id adapter.SessionID) {
					resumes++
					if string(id) != firstSession {
						t.Errorf("resumed %q want %q", id, firstSession)
					}
				}}
				selected.Adapter = dummy.NewWithStart(func(req adapter.HarnessRequest, env adapter.SandboxEnv) error {
					calls++
					workContexts = append(workContexts, req.Context)
					original, err := c.ResolveConfig(env)
					if err != nil {
						return err
					}
					working, err := original.WorkingCopy(t.Context())
					if err != nil {
						return err
					}
					defer working.Close()
					configDirs = append(configDirs, working.ConfigDir())
					lease, err := harnessenv.AcquireAuthentication(t.Context(), auth)
					if err != nil {
						return err
					}
					defer lease.Close()
					if err := lease.Install(working); err != nil {
						return err
					}
					_, credentials, err := working.AuthenticationFile()
					if err != nil {
						return err
					}
					if calls > 1 && !strings.Contains(string(credentials), "after-answer") {
						return fmt.Errorf("stale authentication")
					}
					state, err := os.ReadFile(filepath.Join(working.ConfigDir(), "session-state"))
					if resumes > 0 && (err != nil || string(state) != fmt.Sprint(calls-1)) {
						return fmt.Errorf("session state not restored: %q %v", state, err)
					}
					if calls > 1 && resumes == 0 && !os.IsNotExist(err) {
						return fmt.Errorf("new session received retained state")
					}
					ctx, closeConfig, err := sandbox.WithWorkingConfiguration(t.Context(), working)
					if err != nil {
						return err
					}
					env.ConfigDir, env.Env = working.ConfigDir(), working.Env()
					p, err := c.Launch(ctx, env, []string{"fixture-micro-harness"}, io.Discard, io.Discard)
					if err == nil {
						err = p.Wait()
					}
					closeErr := closeConfig()
					if err != nil {
						return err
					}
					if closeErr != nil {
						return closeErr
					}
					out := filepath.Join(r.dataDir, "executions", logExecution, fmt.Sprintf("work-%d", calls), "out", "question.json")
					return os.WriteFile(out, []byte(`{"question":"Keep or remove?","options":["keep","remove"]}`), 0600)
				})
				registry := &adapter.Registry{}
				if err := registry.Register("codex", selected); err != nil {
					t.Fatal(err)
				}
				return registry
			}
			if err := r.ExecuteClaim(t.Context(), config, claim, factory); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow(`SELECT work_session_id FROM executions WHERE id=?`, logExecution).Scan(&firstSession); err != nil {
				t.Fatal(err)
			}
			retained := filepath.Join(r.dataDir, "work-sessions", claim.Workspace.ID, logExecution, "current")
			info, err := os.Stat(retained)
			if err != nil || info.Mode().Perm() != 0700 {
				t.Fatalf("retained permissions: %v %v", info, err)
			}
			for _, name := range []string{"auth.json", ".credentials.json"} {
				if _, err := os.Stat(filepath.Join(retained, "configuration", name)); !os.IsNotExist(err) {
					t.Fatal("retained authentication", err)
				}
			}
			if scenario == "missing" || scenario == "required" {
				if err := os.RemoveAll(retained); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "missing-file" {
				if err := os.Remove(filepath.Join(retained, "configuration", "session-state")); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "tampered" {
				if err := os.WriteFile(filepath.Join(retained, "configuration", "work", "instructions.md"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "no-id" || scenario == "required-no-id" {
				if _, err := f.db.Exec(`UPDATE executions SET work_session_id=NULL WHERE id=?`, logExecution); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "workspace-missing" {
				if err := os.RemoveAll(claim.Workspace.Path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(auth, []byte(`{"token":"after-answer"}`), 0600); err != nil {
				t.Fatal(err)
			}
			answer := func() {
				t.Helper()
				var decision string
				if err := f.db.QueryRow(`SELECT id FROM owner_decisions WHERE execution_id=? AND status='open' AND kind='question'`, logExecution).Scan(&decision); err != nil {
					t.Fatal(err)
				}
				sessionRequest, _ := http.NewRequest("GET", f.server.URL+"/api/session", nil)
				sessionRequest.AddCookie(f.cookie)
				response, err := r.client.Do(sessionRequest)
				if err != nil {
					t.Fatal(err)
				}
				var session struct {
					CSRF string `json:"csrfToken"`
				}
				err = json.NewDecoder(response.Body).Decode(&session)
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				request, _ := http.NewRequest("POST", f.server.URL+"/api/decisions/"+decision+"/answer", strings.NewReader(`{"action":"answer","text":"keep it","option":"keep"}`))
				request.AddCookie(f.cookie)
				request.Header.Set("Origin", f.server.URL)
				request.Header.Set("X-CSRF-Token", session.CSRF)
				request.Header.Set("Content-Type", "application/json")
				response, err = r.client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				data, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != 200 {
					t.Fatalf("answer: %d %s", response.StatusCode, data)
				}
				job, err := r.claim(t.Context())
				if err != nil || job == nil {
					t.Fatalf("claim %v %v", job, err)
				}
				if err := r.ExecuteClaim(t.Context(), config, job.Claim, factory); err != nil {
					t.Fatal(err)
				}
			}
			answer()
			wantStatus, wantClass := "blocked", ""
			if strings.HasPrefix(scenario, "required") {
				wantStatus, wantClass = "failed", "work_failed"
			}
			if scenario == "tampered" {
				wantStatus, wantClass = "failed", "config_error"
			}
			var status, class string
			if err := f.db.QueryRow(`SELECT status,coalesce(error_class,'') FROM executions WHERE id=?`, logExecution).Scan(&status, &class); err != nil {
				t.Fatal(err)
			}
			if status != wantStatus || class != wantClass {
				t.Fatalf("execution=%s/%s want=%s/%s", status, class, wantStatus, wantClass)
			}
			if scenario == "workspace-missing" {
				var cause string
				if err := f.db.QueryRow(`SELECT cause FROM owner_decisions WHERE execution_id=? AND status='open'`, logExecution).Scan(&cause); err != nil || cause != "workspace_missing" {
					t.Fatalf("cause %q %v", cause, err)
				}
			}
			if scenario == "repeat" {
				answer()
			}
			wantCalls, wantResumes := 2, 0
			if scenario == "resume" {
				wantResumes = 1
			}
			if scenario == "repeat" {
				wantCalls, wantResumes = 3, 2
			}
			if strings.HasPrefix(scenario, "required") || scenario == "tampered" || scenario == "workspace-missing" {
				wantCalls = 1
			}
			if calls != wantCalls || resumes != wantResumes {
				t.Fatalf("calls/resumes=%d/%d want=%d/%d", calls, resumes, wantCalls, wantResumes)
			}
			if calls > 1 {
				var saved string
				if err := f.db.QueryRow(`SELECT work_session_id FROM executions WHERE id=?`, logExecution).Scan(&saved); err != nil {
					t.Fatal(err)
				}
				if (resumes > 0 && saved != firstSession) || (resumes == 0 && saved == firstSession) {
					t.Fatalf("persisted session=%q original=%q resumes=%d", saved, firstSession, resumes)
				}
				if configDirs[0] == configDirs[1] {
					t.Fatal("configuration copy reused")
				}
				if !reflect.DeepEqual(workContexts[0].RevisionIDs, workContexts[1].RevisionIDs) || !strings.Contains(workContexts[1].Instructions, `"text":"keep it"`) {
					t.Fatal("context changed or answer missing")
				}
				if strings.Split(workContexts[0].Instructions, "\n\nStage request (JSON data):\n")[0] != strings.Split(workContexts[1].Instructions, "\n\nStage request (JSON data):\n")[0] {
					t.Fatal("pinned Execution Context changed")
				}
				request, err := os.ReadFile(filepath.Join(r.dataDir, "executions", logExecution, "work-2", "request.json"))
				if err != nil {
					t.Fatal(err)
				}
				var req contract.Request
				if err := json.Unmarshal(request, &req); err != nil {
					t.Fatal(err)
				}
				if req.Answer == nil || req.Answer.Text != "keep it" || req.Answer.Option == nil || *req.Answer.Option != "keep" {
					t.Fatalf("request answer=%+v", req.Answer)
				}
			}
			launches, err := os.ReadFile(filepath.Join(runtimeRoot, "launches"))
			if err != nil {
				t.Fatal(err)
			}
			names := strings.Fields(string(launches))
			unique := map[string]bool{}
			for _, name := range names {
				unique[name] = true
			}
			if len(unique) != wantCalls || len(names) != wantCalls {
				t.Fatalf("Work Sandboxes reused: %q", names)
			}
			var checks int
			if err := f.db.QueryRow(`SELECT count(*) FROM stage_runs WHERE execution_id=? AND stage='check'`, logExecution).Scan(&checks); err != nil || checks != 0 {
				t.Fatalf("questions advanced to check: %d %v", checks, err)
			}
		})
	}
}
