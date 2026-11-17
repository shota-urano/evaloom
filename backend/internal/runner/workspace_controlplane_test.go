package runner

import (
	"context"
	"fmt"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestMissingWorkspaceReachesControlPlane(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		for _, link := range []string{"none", "absolute", "relative", "chain", "ancestor"} {
			t.Run(fmt.Sprintf("resumed=%v/link=%s", resumed, link), func(t *testing.T) {
				f := newLogControlPlane(t)
				r, claim, _, services := stageFixture(t, false)
				process, err := New(f.config)
				if err != nil {
					t.Fatal(err)
				}
				r.controlPlaneURL, r.client, r.token = process.controlPlaneURL, process.client, process.token
				claim.Execution.ID, claim.Execution.TaskID, claim.Task.ID = logExecution, "task-001", "task-001"
				setMissingWorkspace(t, r, &claim, link)
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
				services.Reporter = r
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
}
