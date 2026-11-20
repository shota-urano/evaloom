package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/dummy"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

type failedWorkAdapter struct{ adapter.Adapter }

func (a failedWorkAdapter) Result(id adapter.SessionID) (adapter.HarnessResult, error) {
	result, err := a.Adapter.Result(id)
	result.Status = adapter.ResultFailed
	result.ErrorClass = stagePtr(adapter.ErrorExecutionFailed)
	return result, err
}

// Replace only the micro-harness. Run the production Profile loader, work
// executor, collectors, exclusion, HTTP reporting and Control Plane/SQLite.
// FinishStage's real next response proves whether check may start, without
// needing to run an unrelated check/publish container in this work-stage test.
func TestWorkResultAssembly(t *testing.T) {
	question := `{"question":"どちらにしますか？","options":["残す","消す"]}`
	for _, tt := range []struct {
		name, question, summary, kind string
		failed                        bool
		wantRun, wantNext, wantError  string
	}{
		{name: "success", wantRun: "succeeded", wantNext: "check"},
		{name: "question", question: question, wantRun: "question", wantNext: "stop"},
		{name: "failed with question", question: question, failed: true, wantRun: "failed", wantNext: "stop", wantError: "work_failed"},
		{name: "failed without question", failed: true, wantRun: "failed", wantNext: "stop", wantError: "work_failed"},
		{name: "malformed JSON", question: `{`, wantRun: "failed", wantNext: "stop", wantError: "work_failed"},
		{name: "empty question", question: `{"question":""}`, wantRun: "failed", wantNext: "stop", wantError: "work_failed"},
		{name: "unknown field", question: `{"question":"continue?","status":"succeeded"}`, wantRun: "failed", wantNext: "stop", wantError: "work_failed"},
		{name: "null options", question: `{"question":"continue?","options":[null]}`, wantRun: "failed", wantNext: "stop", wantError: "work_failed"},
		{name: "too many options", question: `{"question":"continue?","options":["1","2","3","4","5","6","7","8","9","10","11"]}`, wantRun: "failed", wantNext: "stop", wantError: "work_failed"},
		{name: "4000 characters", question: `{"question":"` + strings.Repeat("問", 4000) + `"}`, wantRun: "question", wantNext: "stop"},
		{name: "4001 characters", question: `{"question":"` + strings.Repeat("問", 4001) + `"}`, wantRun: "failed", wantNext: "stop", wantError: "work_failed"},
		{name: "64 KiB question", question: `{"question":"continue?"}` + strings.Repeat(" ", maxWorkOutput-len(`{"question":"continue?"}`)), wantRun: "question", wantNext: "stop"},
		{name: "oversized question", question: `{"question":"continue?"}` + strings.Repeat(" ", maxWorkOutput), wantRun: "failed", wantNext: "stop", wantError: "work_failed"},
		{name: "question symlink", kind: "question symlink", wantRun: "failed", wantNext: "stop", wantError: "work_failed"},
		{name: "question directory", kind: "question directory", wantRun: "failed", wantNext: "stop", wantError: "work_failed"},
		{name: "summary", summary: "completed TOKEN CREDENTIAL Authorization: Bearer private-header", wantRun: "succeeded", wantNext: "check"},
		{name: "question and summary", question: question, summary: "completed", wantRun: "question", wantNext: "stop"},
		{name: "summary after answer", question: question, summary: "BEFORE-ANSWER-SUMMARY", wantRun: "question", wantNext: "stop"},
		{name: "oversized summary", summary: strings.Repeat("あ", maxWorkOutput/3+1), wantRun: "succeeded", wantNext: "check"},
		{name: "secret crossing summary limit", summary: strings.Repeat("x", maxWorkOutput-10) + "TOKEN", wantRun: "succeeded", wantNext: "check"},
		{name: "summary symlink", kind: "summary symlink", wantRun: "succeeded", wantNext: "check"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newLogControlPlane(t)
			r, claim, _, _ := stageFixture(t, false)
			process, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			r.controlPlaneURL, r.client, r.token, r.claimURL = process.controlPlaneURL, process.client, process.token, process.claimURL
			if tt.name == "summary" || tt.name == "summary after answer" {
				// Exercise the complete Runner -> artifact API -> outbox path.
				// Only the fake micro-harness writes the deliberately secret text;
				// the Control Plane never receives the Runner's literal secrets.
				t.Cleanup(linearoutbox.SetSummarySource(f.files.HarnessSummaryTx))
				for _, query := range []string{
					`UPDATE tasks SET source='linear' WHERE id='task-001'`,
					`INSERT INTO linear_issue_links (id,linear_issue_id,issue_identifier,issue_url,team_id,task_id,attach_seq,created_at)
					 VALUES ('link','issue','ENG-1','https://linear.app/test','team','task-001',1,'now')`,
				} {
					if _, err := f.db.Exec(query); err != nil {
						t.Fatal(err)
					}
				}
			}
			claim.Execution.ID, claim.Execution.TaskID, claim.Task.ID = logExecution, "task-001", "task-001"
			claim.Profile.HarnessID = "fixture-harness"
			if _, err := f.db.Exec(`UPDATE harness_profiles SET definition=? WHERE id='fixture-profile'`, string(claim.Profile.Definition)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE executions SET status='claimed',profile_id='fixture-profile',profile_commit_sha=?,harness_id='fixture-harness' WHERE id=?`, claim.Profile.CommitSHA, logExecution); err != nil {
				t.Fatal(err)
			}
			if tt.name == "summary after answer" {
				// Pin the workspace and Profile for the real claim after the answer.
				for _, statement := range []struct {
					query string
					args  []any
				}{
					{`UPDATE harnesses SET repo_path=? WHERE id='fixture-harness'`, []any{claim.Profile.RepoPath}},
					{`UPDATE harness_profiles SET commit_sha=? WHERE id='fixture-profile'`, []any{claim.Profile.CommitSHA}},
					{`INSERT INTO task_workspaces(id,task_id,runner_id,path,status,created_by_execution_id,created_at) VALUES (?,'task-001','mac-mini',?,'active',?,'now')`, []any{claim.Workspace.ID, claim.Workspace.Path, logExecution}},
					{`UPDATE executions SET profile_version='1.0.0',route_kind='project',project_id='project',workspace_id=?,params='{}',contract_version='1' WHERE id=?`, []any{claim.Workspace.ID, logExecution}},
				} {
					if _, err := f.db.Exec(statement.query, statement.args...); err != nil {
						t.Fatal(err)
					}
				}
			}
			config := f.config
			auth := filepath.Join(t.TempDir(), "authentication")
			if err := os.WriteFile(auth, []byte(`{"token":"model-private-credential"}`), 0600); err != nil {
				t.Fatal(err)
			}
			config.MicroHarnessAuthFiles = map[string]string{"codex": auth}
			e := &executionExecutor{runner: r, config: config, claim: claim, paths: DefaultProposalPaths, adapters: &adapter.Registry{}}
			var startedSession adapter.SessionID
			var outcome StageOutcome
			var next StageNext
			var directory string
			runWork := func(question, summary string) error {
				return r.WithProfile(t.Context(), claim, func(ctx context.Context, profile LoadedProfile) error {
					var resume stageResume
					if len(claim.Resume) > 0 {
						if err := json.Unmarshal(claim.Resume, &resume); err != nil {
							return err
						}
					}
					request, err := stageRequest(claim, model.StageWork, e.paths, resume)
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
					selected := dummy.NewWithStart(func(req adapter.HarnessRequest, env adapter.SandboxEnv) error {
						// The synchronous test Adapter owns credential transfer just as
						// real Adapters do. Runner must not read outside this lease.
						wiring := e.workAdapterConfig()
						original, err := wiring.ResolveConfig(env)
						if err != nil {
							return err
						}
						working, err := original.WorkingCopy(ctx)
						if err != nil {
							return err
						}
						defer working.Close()
						store, err := wiring.ResolveAuthentication(req)
						if err != nil {
							return err
						}
						lease, err := harnessenv.AcquireAuthentication(ctx, store)
						if err != nil {
							return err
						}
						defer lease.Close()
						if err := lease.Install(working); err != nil {
							return err
						}
						write := func(name, text string) {
							t.Helper()
							if err := os.WriteFile(filepath.Join(directory, "out", name), []byte(text), 0600); err != nil {
								t.Fatal(err)
							}
						}
						// Fake check verdicts and a fake program result must remain opaque,
						// for both successful and failed work.
						write("result.json", `{"stage":"check","status":"failed","errorClass":"check_failed"}`)
						write("check.json", `{"status":"succeeded","contentId":"forged"}`)
						if question != "" {
							write("question.json", question)
						}
						if summary != "" {
							write("summary.md", strings.NewReplacer("TOKEN", r.token, "CREDENTIAL", "model-private-credential").Replace(summary))
						}
						if strings.HasSuffix(tt.kind, "symlink") {
							target := filepath.Join(t.TempDir(), "private")
							if err := os.WriteFile(target, []byte("must not read"), 0600); err != nil {
								t.Fatal(err)
							}
							name := "summary.md"
							if tt.kind == "question symlink" {
								name = "question.json"
							}
							return os.Symlink(target, filepath.Join(directory, "out", name))
						}
						if tt.kind == "question directory" {
							return os.Mkdir(filepath.Join(directory, "out", "question.json"), 0700)
						}
						return nil
					})
					if tt.failed {
						selected = failedWorkAdapter{selected}
					}
					in := StageInvocation{Stage: model.StageWork, Attempt: attempt, Profile: profile,
						Workspace: claim.Workspace.Path, Directory: directory, Adapter: selected, timeout: time.Minute}
					outcome = r.runStageWithSecrets(ctx, logExecution, claim.Profile.HarnessID, in, e)
					if outcome.Work != nil && outcome.Work.SessionID != nil {
						startedSession = adapter.SessionID(*outcome.Work.SessionID)
					}
					next, err = r.FinishStage(ctx, logExecution, model.StageWork, attempt, outcome)
					return err
				})
			}
			err = runWork(tt.question, tt.summary)
			if err != nil {
				t.Fatal(err)
			}
			var run, class, savedSession string
			if err := f.db.QueryRow(`SELECT s.status,coalesce(s.error_class,''),coalesce(e.work_session_id,'') FROM stage_runs s JOIN executions e ON e.id=s.execution_id WHERE s.execution_id=? AND s.stage='work'`, logExecution).Scan(&run, &class, &savedSession); err != nil {
				t.Fatal(err)
			}
			if run != tt.wantRun || next.Next != tt.wantNext || class != tt.wantError || savedSession == "" || savedSession != string(startedSession) {
				t.Fatalf("run=%s next=%s class=%s session=%q outcome=%+v", run, next.Next, class, savedSession, outcome)
			}
			var checks, questions, decisions, summaries, forged int
			for query, dest := range map[string]*int{
				`SELECT count(*) FROM stage_runs WHERE execution_id=? AND stage='check'`:                                           &checks,
				`SELECT count(*) FROM artifacts WHERE execution_id=? AND kind='question'`:                                          &questions,
				`SELECT count(*) FROM owner_decisions WHERE execution_id=? AND kind='question' AND cause='question'`:               &decisions,
				`SELECT count(*) FROM artifacts WHERE execution_id=? AND kind='work_summary'`:                                      &summaries,
				`SELECT count(*) FROM artifacts WHERE execution_id=? AND kind='out_file' AND name IN ('result.json','check.json')`: &forged,
			} {
				if err := f.db.QueryRow(query, logExecution).Scan(dest); err != nil {
					t.Fatal(err)
				}
			}
			wantQuestion := 0
			if tt.question != "" && !tt.failed {
				wantQuestion = 1
			}
			wantDecision := 0
			if tt.wantRun == "question" {
				wantDecision = 1
				var status string
				if err := f.db.QueryRow(`SELECT status FROM executions WHERE id=?`, logExecution).Scan(&status); err != nil || status != "blocked" {
					t.Fatalf("question execution=%s err=%v", status, err)
				}
			}
			if checks != 0 || questions != wantQuestion || decisions != wantDecision || forged != 2 {
				t.Fatalf("checks=%d questions=%d decisions=%d opaque files=%d", checks, questions, decisions, forged)
			}
			if tt.summary == "" {
				if summaries != 0 {
					t.Fatalf("unexpected summary artifacts: %d", summaries)
				}
				return
			}
			var storage, id string
			var redacted bool
			if err := f.db.QueryRow(`SELECT id,storage_path,redacted FROM artifacts WHERE execution_id=? AND kind='work_summary'`, logExecution).Scan(&id, &storage, &redacted); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(f.dataDir, storage))
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Work.SummaryArtifactID == nil || *outcome.Work.SummaryArtifactID != id || strings.Contains(string(data), r.token) || strings.Contains(string(data), "model-private-credential") || strings.Contains(string(data), "private-header") {
				t.Fatalf("summary not linked or sanitized: %q", data)
			}
			if tt.name == "summary" && (!redacted || !strings.Contains(string(data), "[REDACTED]")) {
				t.Fatalf("summary redaction metadata=%v data=%q", redacted, data)
			}
			if tt.name == "summary after answer" {
				var decision string
				if err := f.db.QueryRow(`SELECT id FROM owner_decisions WHERE execution_id=? AND status='open' AND kind='question'`, logExecution).Scan(&decision); err != nil {
					t.Fatal(err)
				}
				request, _ := http.NewRequest("GET", f.server.URL+"/api/session", nil)
				request.AddCookie(f.cookie)
				response, err := r.client.Do(request)
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
				request, _ = http.NewRequest("POST", f.server.URL+"/api/decisions/"+decision+"/answer", strings.NewReader(`{"action":"answer","text":"残す","option":"残す"}`))
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
				if response.StatusCode != http.StatusOK {
					t.Fatalf("answer: %d %s", response.StatusCode, data)
				}
				job, err := r.claim(t.Context())
				if err != nil || job == nil {
					t.Fatalf("claim: %v %v", job, err)
				}
				claim = job.Claim
				e.claim = claim
				if claim.Execution.ID != logExecution || claim.Execution.ResumeStage == nil || *claim.Execution.ResumeStage != "work" {
					t.Fatalf("answer did not resume the same Execution: %+v", claim.Execution)
				}
				if err := runWork("", "completed TOKEN CREDENTIAL Authorization: Bearer private-header"); err != nil {
					t.Fatal(err)
				}
				if next.Next != "check" {
					t.Fatalf("resumed work next=%s", next.Next)
				}
				if err := f.db.QueryRow(`SELECT count(*) FROM artifacts WHERE execution_id=? AND kind='work_summary'`, logExecution).Scan(&summaries); err != nil || summaries != 2 {
					t.Fatalf("resumed summaries=%d err=%v", summaries, err)
				}
			}
			if tt.name == "summary" || tt.name == "summary after answer" {
				// Start/finish the real check report path. The fake check executor
				// deliberately echoes the Runner token into its result summary.
				err := r.WithProfile(t.Context(), claim, func(ctx context.Context, profile LoadedProfile) error {
					request, err := stageRequest(claim, model.StageCheck, e.paths, stageResume{})
					if err != nil {
						return err
					}
					attempt, err := r.StartStage(ctx, logExecution, model.StageCheck, request)
					if err != nil {
						return err
					}
					directory, err := r.prepareStageFiles(logExecution, model.StageCheck, attempt, request, nil)
					if err != nil {
						return err
					}
					result, err := json.Marshal(map[string]any{
						"contractVersion": "1", "stage": "check", "status": "failed", "errorClass": "check_failed",
						"message": "checks failed", "summary": "check summary " + r.token + " path=" + directory,
						"findings": []map[string]any{{"rule": "check_failed", "severity": "error", "message": "FINDING-BODY-CANARY"}},
						"usage":    nil,
					})
					if err != nil {
						return err
					}
					in := StageInvocation{Stage: model.StageCheck, Attempt: attempt, Profile: profile,
						Workspace: claim.Workspace.Path, Directory: directory, timeout: time.Minute}
					outcome := r.runStageWithSecrets(ctx, logExecution, claim.Profile.HarnessID, in,
						stageExecutorFunc(func(context.Context, StageInvocation) StageOutcome {
							return StageOutcome{ExitCode: stagePtr(1), Result: result}
						}))
					_, err = r.FinishStage(ctx, logExecution, model.StageCheck, attempt, outcome)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				var body string
				if err := f.db.QueryRow(`SELECT body FROM linear_outbox WHERE action='comment' AND kind='failed'`).Scan(&body); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(body, "作業概要:\ncompleted [REDACTED] [REDACTED] Authorization: [REDACTED]") ||
					!strings.Contains(body, "check summary [REDACTED] path=[REDACTED]") ||
					!strings.Contains(body, "finding 件数: error=1, warning=0, info=0") ||
					strings.Contains(body, r.token) || strings.Contains(body, "model-private-credential") || strings.Contains(body, "private-header") ||
					strings.Contains(body, "FINDING-BODY-CANARY") || strings.Contains(body, r.dataDir) {
					t.Fatalf("Linear body did not exclude Runner secrets: %s", body)
				}
				if strings.Contains(body, "BEFORE-ANSWER-SUMMARY") || strings.Contains(body, "判断待ち ID:") {
					t.Fatalf("Linear body retained obsolete work or an answered question: %s", body)
				}
			}
			if tt.name == "secret crossing summary limit" && strings.Contains(string(data), r.token[:10]) {
				t.Fatal("truncation retained a credential prefix")
			}
			if len(tt.summary) > maxWorkOutput && (!strings.Contains(string(data), "Truncated") || !utf8.Valid(data) || len(data) > maxWorkOutput+100) {
				t.Fatalf("oversized summary not truncated safely: %d bytes", len(data))
			}
			retained, err := os.ReadFile(filepath.Join(directory, "out", "summary.md"))
			if err != nil || strings.Contains(string(retained), r.token) || strings.Contains(string(retained), "model-private-credential") {
				t.Fatalf("retained summary not sanitized: %v", err)
			}
		})
	}
}
