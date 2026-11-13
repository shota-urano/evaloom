package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/dummy"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Spec 08 3.2 / condition 31: the same Profile and execution wiring must run a
// separately packaged Adapter by name after adding only its registration. The
// dummy is in-memory; this tests extension and real Control Plane transitions,
// not CLI execution or Sandbox isolation.
func TestDummyAdapterExtensionExecution(t *testing.T) {
	for _, registered := range []bool{false, true} {
		name := "unregistered"
		if registered {
			name = "registered"
		}
		t.Run(name, func(t *testing.T) {
			f := newLogControlPlane(t)
			r, claim, _, services := stageFixture(t, false)
			process, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			r.controlPlaneURL, r.client, r.token = process.controlPlaneURL, process.client, process.token
			claim.Execution.ID, claim.Execution.TaskID, claim.Task.ID = logExecution, "task-001", "task-001"
			var def contract.ProfileDefinition
			if err := json.Unmarshal(claim.Profile.Definition, &def); err != nil {
				t.Fatal(err)
			}
			def.Stages.Work.MicroHarness = "extension_test"
			claim.Profile.Definition, err = json.Marshal(def)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE harness_profiles SET definition = ? WHERE id = 'fixture-profile'`, string(claim.Profile.Definition)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE executions SET status = 'claimed', profile_id = 'fixture-profile',
				route_kind = 'project', project_id = 'project', harness_id = 'fixture-harness' WHERE id = ?`, logExecution); err != nil {
				t.Fatal(err)
			}
			services.Adapters = &adapter.Registry{}
			if registered {
				// The only Adapter-specific wiring; no Runner or Control Plane changes.
				if err := services.Adapters.Register("extension_test", dummy.New()); err != nil {
					t.Fatal(err)
				}
			}
			services.Reporter = workspaceHTTPReporter{t: t, r: r}
			var stages []model.Stage
			var sessionID adapter.SessionID
			services.Executor = stageExecutorFunc(func(ctx context.Context, in StageInvocation) StageOutcome {
				stages = append(stages, in.Stage)
				if in.Stage != model.StageWork {
					if in.Adapter != nil {
						t.Fatal("program stage received a work Adapter")
					}
					// Deterministic special-harness results; the real Control Plane
					// validates them and decides whether the next stage may run.
					outcome := successfulStage(in.Stage)
					if in.Stage == model.StageCheck {
						outcome.Result = json.RawMessage(`{"contractVersion":"1","stage":"check","status":"succeeded","message":"checked","contentId":"extension-content"}`)
					}
					return outcome
				}
				raw, err := os.ReadFile(filepath.Join(in.Directory, "request.json"))
				if err != nil {
					t.Fatal(err)
				}
				request := readStageRequest(t, raw)
				// Use the existing shared Adapter request at the executor boundary;
				// this conversion does not depend on the implementation's name.
				req := adapter.HarnessRequest{
					ContractVersion: request.ContractVersion, ExecutionID: request.Execution.ID,
					Task: adapter.RequestTask{ID: request.Task.ID, Title: request.Task.Title,
						Body: request.Task.Body, AcceptanceCriteria: request.Task.AcceptanceCriteria},
					Project: adapter.RequestProject{Workdir: in.Workspace},
					Profile: adapter.RequestProfile{HarnessID: request.Profile.HarnessID,
						Version: request.Profile.Version, CommitSHA: request.Profile.CommitSHA,
						ModelID: in.Profile.Definition.Stages.Work.Model, Definition: claim.Profile.Definition},
					Context: adapter.RequestContext{RevisionIDs: []string{}},
					Limits:  adapter.RequestLimits{TimeoutSec: request.Limits.TimeoutSec},
				}
				sessionID, err = in.Adapter.Start(ctx, req, adapter.SandboxEnv{Worktree: in.Workspace})
				if err != nil {
					t.Fatal(err)
				}
				result, err := in.WaitWork(ctx, in.Adapter, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				if result.Status != adapter.ResultSucceeded || result.ErrorClass != nil || result.SessionID != sessionID {
					t.Fatalf("Adapter did not succeed: %+v", result)
				}
				return StageOutcome{ExitCode: stagePtr(0), Work: &StageWork{SessionID: stagePtr(string(result.SessionID))}}
			})
			if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
				t.Fatal(err)
			}
			var status, taskStatus, savedSession, errorClass string
			if err := f.db.QueryRow(`SELECT e.status, t.status, coalesce(e.work_session_id, ''), coalesce(e.error_class, '')
				FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, logExecution).
				Scan(&status, &taskStatus, &savedSession, &errorClass); err != nil {
				t.Fatal(err)
			}
			if !registered {
				if status != "failed" || taskStatus != "failed" || errorClass != "config_error" || len(stages) != 0 || savedSession != "" {
					t.Fatalf("unregistered Adapter reached execution: status=%s task=%s error=%s stages=%v session=%q", status, taskStatus, errorClass, stages, savedSession)
				}
				return
			}
			if status != "succeeded" || taskStatus != "completed" || errorClass != "" || sessionID == "" || savedSession != string(sessionID) {
				t.Fatalf("execution did not complete with Adapter session: status=%s task=%s error=%s session=%q want=%q", status, taskStatus, errorClass, savedSession, sessionID)
			}
			wantStages := []model.Stage{model.StageWork, model.StageCheck, model.StagePublish}
			if !reflect.DeepEqual(stages, wantStages) {
				t.Fatalf("stages = %v, want %v", stages, wantStages)
			}
			var succeeded int
			if err := f.db.QueryRow(`SELECT count(*) FROM stage_runs WHERE execution_id = ? AND status = 'succeeded'`, logExecution).Scan(&succeeded); err != nil || succeeded != 3 {
				t.Fatalf("successful stage runs = %d, want 3: %v", succeeded, err)
			}
		})
	}
}
