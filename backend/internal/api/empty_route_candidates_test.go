package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Q222: exercise proposal creation and the real claim endpoint with a non-null
// proposer model, so empty candidates alone must prevent Runner dispatch.
func TestRouteProposalDispatchWithCandidateCounts(t *testing.T) {
	for _, routes := range []struct {
		name       string
		setup      func(*testing.T, *authFixture)
		candidates int
	}{
		{"no routes", func(t *testing.T, f *authFixture) {}, 0},
		{"only harnesses without defaults", func(t *testing.T, f *authFixture) {
			status, body := projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-b"))
			requireHarness(t, status, body, 201)
			stageExec(t, f, `UPDATE harnesses SET default_profile_id=NULL`)
			stageExec(t, f, `INSERT INTO routes VALUES ('route:a',NULL,'harness-a','t','t'),('route:b',NULL,'harness-b','t','t')`)
		}, 0},
		{"one project", func(t *testing.T, f *authFixture) {
			stageExec(t, f, `INSERT INTO routes VALUES ('route:a','project-a',NULL,'t','t')`)
		}, 1},
		{"one harness with default", func(t *testing.T, f *authFixture) {
			stageExec(t, f, `INSERT INTO routes VALUES ('route:a',NULL,'harness-a','t','t')`)
		}, 1},
		{"multiple candidates", func(t *testing.T, f *authFixture) {
			stageExec(t, f, `INSERT INTO routes VALUES ('route:a','project-a',NULL,'t','t'),('route:b',NULL,'harness-a','t','t')`)
		}, 2},
	} {
		for _, trigger := range []struct {
			name   string
			labels []string
		}{
			{"no_label", nil},
			{"unknown_label", []string{"route:unknown"}},
			{"multiple_labels", []string{"route:a", "route:b"}},
		} {
			t.Run(routes.name+"/"+trigger.name, func(t *testing.T) {
				f, token, profiles := claimFixture(t)
				enableDirectRouting(t, f, profiles[0].ID, "claim-runner")
				stageExec(t, f, `DELETE FROM routes`)
				routes.setup(t, f)
				tx, err := f.db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				starter := &linearintake.TaskStarter{}
				decision, err := starter.StartLinearTask(context.Background(), tx,
					recheckIssue("empty-candidates", "team-a", trigger.labels...), 1)
				if err != nil || decision != linearintake.DecisionRouting {
					t.Fatalf("intake decision = %q, err = %v", decision, err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				var proposalID, taskID, gotTrigger, status string
				var candidateCount int
				if err := f.db.QueryRow(`SELECT id, task_id, trigger, status, json_array_length(candidates)
					FROM route_proposals`).Scan(&proposalID, &taskID, &gotTrigger, &status, &candidateCount); err != nil {
					t.Fatal(err)
				}
				if gotTrigger != trigger.name || candidateCount != routes.candidates {
					t.Fatalf("trigger = %s, candidates = %d", gotTrigger, candidateCount)
				}
				if n := stageCount(t, f, `SELECT count(*) FROM tasks WHERE id=? AND status='routing'
					AND project_id IS NULL AND harness_id IS NULL AND intake_block_cause IS NULL`, taskID); n != 1 {
					t.Fatal("task must wait for routing")
				}
				if n := stageCount(t, f, `SELECT count(*) FROM executions WHERE task_id=?`, taskID); n != 0 {
					t.Fatal("unapproved routing created an Execution")
				}
				if routes.candidates == 0 {
					// Check before claim: intake itself must fail and open the decision.
					requireUndispatchedRouteProposal(t, f, proposalID, taskID)
					for range 2 {
						code, body := requestClaim(t, f, token)
						if kind := claimJobKind(t, code, body); kind != "" {
							t.Fatalf("empty candidates dispatched %s: %s", kind, body)
						}
					}
					requireUndispatchedRouteProposal(t, f, proposalID, taskID)
					return
				}
				if status != "queued" || stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE task_id=?`, taskID) != 0 {
					t.Fatalf("proposal must be queued without a decision, status = %s", status)
				}
				code, body := requestClaim(t, f, token)
				if kind := claimJobKind(t, code, body); kind != "route_proposal" {
					t.Fatalf("nonempty candidates not dispatched: %s", body)
				}
				var response struct {
					Job store.RouteProposalJob `json:"job"`
				}
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatal(err)
				}
				var candidates []store.RouteCandidate
				if err := json.Unmarshal(response.Job.Proposal.Candidates, &candidates); err != nil {
					t.Fatal(err)
				}
				if response.Job.Proposal.ID != proposalID || len(candidates) != routes.candidates {
					t.Fatalf("wrong proposal job: %s", body)
				}
				if n := stageCount(t, f, `SELECT count(*) FROM route_proposals WHERE id=? AND status='running' AND runner_id='claim-runner'`, proposalID); n != 1 {
					t.Fatal("proposal was not claimed")
				}
			})
		}
	}
}

func requireUndispatchedRouteProposal(t *testing.T, f *authFixture, proposalID, taskID string) {
	t.Helper()
	if n := stageCount(t, f, `SELECT count(*) FROM route_proposals WHERE id=? AND status='failed'
		AND candidates='[]' AND error_class IS NULL AND runner_id IS NULL AND proposed_label IS NULL
		AND reason IS NULL AND finished_at IS NOT NULL`, proposalID); n != 1 {
		t.Fatal("empty proposal must fail without an error class or a Runner")
	}
	decisionID := routeDecisionOf(t, f, proposalID)
	requireDecisionOpenComment(t, f, taskID, decisionID)
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE task_id=?`, taskID); n != 1 {
		t.Fatalf("decisions = %d, want 1", n)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=?`, taskID); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
}

// Queued rows from before Q222 must also never reach a Runner. Skipping them
// must still let the next eligible proposal be dispatched in the same claim.
func TestClaimFailsPreviouslyQueuedEmptyCandidates(t *testing.T) {
	for _, withNext := range []bool{false, true} {
		name := "no next job"
		if withNext {
			name = "next proposal"
		}
		t.Run(name, func(t *testing.T) {
			f, token, profiles := claimFixture(t)
			enableDirectRouting(t, f, profiles[0].ID, "claim-runner")
			insertRouteProposal(t, f, "empty", "task-empty", "2026-11-01T00:00:00Z")
			stageExec(t, f, `UPDATE route_proposals SET candidates='[]' WHERE id='empty'`)
			stageExec(t, f, `INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id, task_id, attach_seq, created_at)
				VALUES ('link-empty', 'issue-empty', 'ENG-1', 'https://linear.app/x/ENG-1', 'team-a', 'task-empty', 1, 't')`)
			wantKind := ""
			if withNext {
				insertRouteProposal(t, f, "next", "task-next", "2026-11-02T00:00:00Z")
				wantKind = "route_proposal"
			}
			code, body := requestClaim(t, f, token)
			if kind := claimJobKind(t, code, body); kind != wantKind {
				t.Fatalf("claim kind = %q, want %q: %s", kind, wantKind, body)
			}
			requireUndispatchedRouteProposal(t, f, "empty", "task-empty")
			if withNext && stageCount(t, f, `SELECT count(*) FROM route_proposals WHERE id='next' AND status='running' AND runner_id='claim-runner'`) != 1 {
				t.Fatal("next eligible proposal was not claimed")
			}
		})
	}
}
