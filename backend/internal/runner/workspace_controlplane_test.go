package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Adapt the existing stage reporting boundary to the real API for this test.
// Production delivery/retry wiring belongs to the command integration task.
type workspaceHTTPReporter struct {
	t *testing.T
	r *Runner
}

func (s workspaceHTTPReporter) post(ctx context.Context, id string, input, reply any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.r.controlPlaneURL+"/api/executions/"+id+"/result", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.r.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := s.r.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("report HTTP %d: %s", response.StatusCode, body)
	}
	return json.NewDecoder(response.Body).Decode(reply)
}

func (s workspaceHTTPReporter) StartStage(ctx context.Context, id string, stage model.Stage, request []byte) (int, error) {
	path := filepath.Join(s.t.TempDir(), "request.json")
	if err := os.WriteFile(path, request, 0600); err != nil {
		return 0, err
	}
	artifactID := ulid.Make().String()
	if err := s.r.registerArtifact(ctx, LogArtifact{ID: artifactID, ExecutionID: id, Stage: string(stage), Kind: "request_json", Name: "request.json", Path: path}); err != nil {
		return 0, err
	}
	var reply struct{ Attempt int }
	err := s.post(ctx, id, map[string]any{"type": "stage_started", "stage": stage, "requestArtifactId": artifactID}, &reply)
	return reply.Attempt, err
}

func (s workspaceHTTPReporter) FinishStage(ctx context.Context, id string, stage model.Stage, attempt int, outcome StageOutcome) (StageNext, error) {
	input := struct {
		Type    string      `json:"type"`
		Stage   model.Stage `json:"stage"`
		Attempt int         `json:"attempt"`
		StageOutcome
	}{"stage_finished", stage, attempt, outcome}
	var reply StageNext
	err := s.post(ctx, id, input, &reply)
	return reply, err
}

func TestMissingWorkspaceReachesControlPlane(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(fmt.Sprintf("resumed=%v", resumed), func(t *testing.T) {
			f := newLogControlPlane(t)
			r, claim, _, services := stageFixture(t, false)
			process, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			r.controlPlaneURL, r.client, r.token = process.controlPlaneURL, process.client, process.token
			claim.Execution.ID, claim.Execution.TaskID, claim.Task.ID = logExecution, "task-001", "task-001"
			claim.Workspace.ID = "missing"
			claim.Workspace.Path = filepath.Join(r.workspaceRoot, claim.Workspace.ID)
			var resume any
			if resumed {
				resume = "work"
				setStageResume(t, &claim, stageResume{Stage: model.StageWork})
			}
			if _, err := f.db.Exec(`UPDATE harness_profiles SET definition = ? WHERE id = 'fixture-profile'`, string(claim.Profile.Definition)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE executions SET status = 'claimed', profile_id = 'fixture-profile', resume_stage = ? WHERE id = ?`, resume, logExecution); err != nil {
				t.Fatal(err)
			}
			services.Reporter = workspaceHTTPReporter{t: t, r: r}
			services.Executor = stageExecutorFunc(func(context.Context, StageInvocation) StageOutcome {
				t.Fatal("missing workspace must not run any stage body")
				return StageOutcome{}
			})
			if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
				t.Fatal(err)
			}
			var status, taskStatus, errorClass string
			if err := f.db.QueryRow(`SELECT e.status, t.status, coalesce(e.error_class, '') FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, logExecution).
				Scan(&status, &taskStatus, &errorClass); err != nil {
				t.Fatal(err)
			}
			if resumed {
				if status != "blocked" || taskStatus != "blocked" || errorClass != "" {
					t.Fatalf("resume not blocked: execution=%s task=%s error=%s", status, taskStatus, errorClass)
				}
				var cause string
				if err := f.db.QueryRow(`SELECT cause FROM owner_decisions WHERE execution_id = ? AND kind = 'blocked' AND status = 'open'`, logExecution).Scan(&cause); err != nil || cause != "workspace_missing" {
					t.Fatalf("missing workspace decision: cause=%s err=%v", cause, err)
				}
			} else if status != "failed" || taskStatus != "failed" || errorClass != "config_error" {
				t.Fatalf("initial execution not failed: execution=%s task=%s error=%s", status, taskStatus, errorClass)
			}
			var reports int
			if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = 'stage_finished'`, logExecution).Scan(&reports); err != nil || reports != 1 {
				t.Fatalf("must report exactly one stage end: reports=%d err=%v", reports, err)
			}
		})
	}
}
