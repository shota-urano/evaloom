package api

import (
	"database/sql"
	"testing"
)

// Spec 19 3.9 / 15 Q157: a route decision checks the approved destination's
// Profile only after approval. A required Profile holds without an Execution;
// optional queues, preserving the criteria extracted at intake.
func TestRouteApprovalChecksAcceptanceCriteria(t *testing.T) {
	for _, kind := range []string{"project", "harness"} {
		for _, policy := range []string{"required", "optional"} {
			for _, criteria := range []string{"", "- verified"} {
				t.Run(kind+"/"+policy+"/"+criteria, func(t *testing.T) {
					f, token, proposalID, taskID := routeProposalFixture(t)
					label := "route:a"
					stageExec(t, f, `UPDATE harness_profiles SET definition=json_set(definition, '$.acceptanceCriteria', ?)
						WHERE id=(SELECT profile_id FROM projects WHERE id='project-a')`, policy)
					if kind == "harness" {
						stageExec(t, f, `UPDATE harnesses SET default_profile_id=(SELECT profile_id FROM projects WHERE id='project-a') WHERE id='harness-a'`)
						stageExec(t, f, `INSERT INTO routes VALUES ('route:direct',NULL,'harness-a','t','t')`)
						label = "route:direct"
					}
					stageExec(t, f, `UPDATE tasks SET acceptance_criteria=? WHERE id=?`, criteria, taskID)
					if status, body := proposalResult(t, f, token, proposalID, `{"label":"route:a","reason":"fits A","errorClass":null}`); status != 200 {
						t.Fatalf("result=%d %s", status, body)
					}
					decisionID := routeDecisionOf(t, f, proposalID)
					if n := stageCount(t, f, `SELECT count(*) FROM tasks WHERE id=? AND status='routing' AND intake_block_cause IS NULL`, taskID); n != 1 {
						t.Fatal("criteria checked before approval")
					}
					answer(t, f, decisionID, `{"action":"approve","label":"`+label+`","basis":"`+proposalID+`"}`, 200)
					wantStatus, wantCause, wantExecutions := "queued", "", 1
					if policy == "required" && criteria == "" {
						wantStatus, wantCause, wantExecutions = "blocked", "missing_acceptance_criteria", 0
					}
					var status, stored string
					var cause, project, harness sql.NullString
					if err := f.db.QueryRow(`SELECT status, intake_block_cause, acceptance_criteria, project_id, harness_id FROM tasks WHERE id=?`, taskID).
						Scan(&status, &cause, &stored, &project, &harness); err != nil {
						t.Fatal(err)
					}
					if status != wantStatus || cause.String != wantCause || cause.Valid != (wantCause != "") || stored != criteria {
						t.Fatalf("task=%s/%v criteria=%q; want %s/%s criteria=%q", status, cause, stored, wantStatus, wantCause, criteria)
					}
					if kind == "project" && (project.String != "project-a" || harness.Valid) ||
						kind == "harness" && (project.Valid || harness.String != "harness-a") {
						t.Fatalf("destination=%v/%v", project, harness)
					}
					if n := stageCount(t, f, `SELECT count(*) FROM executions WHERE task_id=?`, taskID); n != wantExecutions {
						t.Fatalf("executions=%d; want %d", n, wantExecutions)
					}
					wantComments := 0
					if wantCause != "" {
						wantComments = 1
					}
					if n := stageCount(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND kind='missing_acceptance_criteria'
						AND action='comment' AND execution_id IS NULL`, taskID); n != wantComments {
						t.Fatalf("criteria comments=%d; want %d", n, wantComments)
					}
					if n := stageCount(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND action='add_label' AND label_name=?`, taskID, label); n != 1 {
						t.Fatal("approved label was not queued")
					}
					requireAnswered(t, f, decisionID)
					requireProposal(t, f, proposalID, "decided", "route:a", nil)
				})
			}
		}
	}
}

func TestRouteApprovalCriteriaHoldIsAtomic(t *testing.T) {
	f, token, proposalID, taskID := routeProposalFixture(t)
	stageExec(t, f, `UPDATE harness_profiles SET definition=json_set(definition, '$.acceptanceCriteria', 'required')`)
	if status, body := proposalResult(t, f, token, proposalID, `{"label":"route:a","reason":"fits A","errorClass":null}`); status != 200 {
		t.Fatalf("result=%d %s", status, body)
	}
	decisionID := routeDecisionOf(t, f, proposalID)
	stageExec(t, f, `CREATE TRIGGER reject_criteria_comment BEFORE INSERT ON linear_outbox
		WHEN NEW.kind='missing_acceptance_criteria' BEGIN SELECT RAISE(ABORT, 'comment write failed'); END`)
	answer(t, f, decisionID, `{"action":"approve","label":"route:a","basis":"`+proposalID+`"}`, 500)
	if n := stageCount(t, f, `SELECT count(*) FROM tasks WHERE id=? AND status='routing' AND intake_block_cause IS NULL
		AND project_id IS NULL AND harness_id IS NULL`, taskID); n != 1 {
		t.Fatal("failed hold changed task")
	}
	requireProposal(t, f, proposalID, "proposed", "route:a", nil)
	if got := routeDecisionOf(t, f, proposalID); got != decisionID {
		t.Fatal("failed hold changed decision")
	}
	// Only the decision_open comment of the opened decision remains.
	if n := stageCount(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND kind<>'decision_open'`, taskID) +
		stageCount(t, f, `SELECT count(*) FROM executions WHERE task_id=?`, taskID); n != 0 {
		t.Fatal("failed hold left side effects")
	}
}

func TestRouteApprovalWithoutDefaultProfileHolds(t *testing.T) {
	f, token, proposalID, taskID := routeProposalFixture(t)
	stageExec(t, f, `UPDATE harnesses SET default_profile_id=NULL WHERE id='harness-a'`)
	stageExec(t, f, `INSERT INTO routes VALUES ('route:direct',NULL,'harness-a','t','t')`)
	if status, body := proposalResult(t, f, token, proposalID, `{"label":"route:a","reason":"fits A","errorClass":null}`); status != 200 {
		t.Fatalf("result=%d %s", status, body)
	}
	decisionID := routeDecisionOf(t, f, proposalID)
	answer(t, f, decisionID, `{"action":"approve","label":"route:direct","basis":"`+proposalID+`"}`, 200)
	if n := stageCount(t, f, `SELECT count(*) FROM tasks WHERE id=? AND status='blocked' AND project_id IS NULL
		AND harness_id='harness-a' AND intake_block_cause='config_missing_profile'`, taskID); n != 1 {
		t.Fatal("missing default did not hold the approved task")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM executions WHERE task_id=?`, taskID) +
		stageCount(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND action='comment' AND kind<>'decision_open'`, taskID); n != 0 {
		t.Fatal("missing default created an Execution or a criteria comment")
	}
	requireAnswered(t, f, decisionID)
}
