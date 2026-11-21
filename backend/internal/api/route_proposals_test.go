package api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"context"
	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
)

// routeProposalFixture is a Linear task in routing with a proposal handed to
// claim-runner (spec 04 3.7) and a route for project-a and project-b.
func routeProposalFixture(t *testing.T) (*authFixture, string, string, string) {
	t.Helper()
	f, token, _ := claimFixture(t)
	proposalID, taskID := ulid.Make().String(), ulid.Make().String()
	insertRouteProposal(t, f, proposalID, taskID, "2026-11-01T00:00:00Z")
	stageExec(t, f, `UPDATE route_proposals SET status='running', runner_id='claim-runner',
		candidates='[{"label":"route:a","kind":"project","id":"project-a","description":"A"},{"label":"route:b","kind":"project","id":"project-b","description":"B"}]'
		WHERE id=?`, proposalID)
	stageExec(t, f, `DELETE FROM routes`)
	stageExec(t, f, `INSERT INTO routes VALUES ('route:a','project-a',NULL,'t','t'),('route:b','project-b',NULL,'t','t')`)
	stageExec(t, f, `INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id, task_id, attach_seq, created_at)
		VALUES (?, 'issue-1', 'ENG-1', 'https://linear.app/x/issue/ENG-1', 'team-a', ?, 1, 't')`, ulid.Make().String(), taskID)
	return f, token, proposalID, taskID
}

func proposalResult(t *testing.T, f *authFixture, token, id, body string) (int, []byte) {
	t.Helper()
	return f.request(t, "POST", "/api/route-proposals/"+id+"/result", false, "", func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Body = io.NopCloser(strings.NewReader(body))
		r.ContentLength = int64(len(body))
	})
}

func routeDecisionOf(t *testing.T, f *authFixture, proposalID string) string {
	t.Helper()
	var id string
	if err := f.db.QueryRow(`SELECT d.id FROM owner_decisions d JOIN route_proposals p ON p.decision_id = d.id
		WHERE p.id = ? AND d.kind = 'route' AND d.status = 'open' AND d.subject_id = p.id AND d.basis = p.id
		AND d.execution_id IS NULL`, proposalID).Scan(&id); err != nil {
		t.Fatalf("route decision of %s: %v", proposalID, err)
	}
	return id
}

// requireDecisionOpenComment checks the "管理画面で判断待ち" comment of a route
// decision (spec 15 3.6.4 step 5, 16 3.2): one comment row without an
// Execution (16 3.4.2), deduplicated by the decision ID.
func requireDecisionOpenComment(t *testing.T, f *authFixture, taskID, decisionID string) {
	t.Helper()
	if n := stageCount(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND kind='decision_open' AND action='comment'
		AND dedupe_key=? AND execution_id IS NULL AND status='pending' AND body LIKE '%'||?||'%'`,
		taskID, taskID+":decision_open:"+decisionID, decisionID); n != 1 {
		t.Fatalf("decision_open comments for %s = %d", decisionID, n)
	}
}

func requireProposal(t *testing.T, f *authFixture, id, status string, label, errorClass any) {
	t.Helper()
	var gotStatus string
	var gotLabel, gotError any
	if err := f.db.QueryRow(`SELECT status, proposed_label, error_class FROM route_proposals WHERE id=?`, id).Scan(&gotStatus, &gotLabel, &gotError); err != nil {
		t.Fatal(err)
	}
	if gotStatus != status || gotLabel != label || gotError != errorClass {
		t.Fatalf("proposal = %s %v %v; want %s %v %v", gotStatus, gotLabel, gotError, status, label, errorClass)
	}
}

func TestRouteProposalProposedThenApproveQueuesTask(t *testing.T) {
	f, token, proposalID, taskID := routeProposalFixture(t)
	status, body := proposalResult(t, f, token, proposalID, `{"label":"route:a","reason":"fits A","errorClass":null}`)
	if status != 200 || !strings.Contains(string(body), `"status":"proposed"`) {
		t.Fatalf("result=%d %s", status, body)
	}
	requireProposal(t, f, proposalID, "proposed", "route:a", nil)
	decisionID := routeDecisionOf(t, f, proposalID)
	requireDecisionOpenComment(t, f, taskID, decisionID)
	// The same result again is accepted without a second decision; another is a conflict.
	status, body = proposalResult(t, f, token, proposalID, `{"label":"route:a","reason":"fits A","errorClass":null}`)
	if status != 200 {
		t.Fatalf("resend=%d %s", status, body)
	}
	status, body = proposalResult(t, f, token, proposalID, `{"label":"route:b","reason":"fits B","errorClass":null}`)
	assertAuthError(t, status, body, 409, Conflict)
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE task_id=?`, taskID); n != 1 {
		t.Fatalf("decisions = %d", n)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM executions WHERE task_id=?`, taskID); n != 0 {
		t.Fatal("Execution before approval")
	}
	// Q112: the owner may approve another routes label than the proposal.
	status, body = approvalRequest(t, f, "POST", "/api/decisions/"+decisionID+"/answer", `{"action":"approve","label":"route:b","basis":"`+proposalID+`"}`)
	if status != 200 {
		t.Fatalf("approve=%d %s", status, body)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM tasks WHERE id=? AND status='queued' AND project_id='project-b' AND harness_id IS NULL`, taskID); n != 1 {
		t.Fatal("task not queued with the approved destination")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM executions WHERE task_id=? AND seq=1 AND mode='continue' AND status='pending'`, taskID); n != 1 {
		t.Fatal("no seq=1 pending Execution")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM executions WHERE task_id=?`, taskID); n != 1 {
		t.Fatal("extra Executions")
	}
	requireProposal(t, f, proposalID, "decided", "route:a", nil)
	if n := stageCount(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND action='add_label' AND kind='route_approved'
		AND label_name='route:b' AND dedupe_key=? AND body IS NULL AND state_name IS NULL AND execution_id IS NULL`, taskID, taskID+":label:"+decisionID); n != 1 {
		t.Fatal("no add_label outbox row")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE id=? AND status='answered'`, decisionID); n != 1 {
		t.Fatal("decision not answered")
	}
	// The answered decision cannot be answered again.
	status, body = approvalRequest(t, f, "POST", "/api/decisions/"+decisionID+"/answer", `{"action":"reject"}`)
	assertAuthError(t, status, body, 409, Conflict)
}

func TestRouteApprovalRollsBackWhenLabelOutboxWriteFails(t *testing.T) {
	f, token, proposalID, taskID := routeProposalFixture(t)
	if status, body := proposalResult(t, f, token, proposalID, `{"label":"route:a","reason":"fits A","errorClass":null}`); status != 200 {
		t.Fatalf("result=%d %s", status, body)
	}
	decisionID := routeDecisionOf(t, f, proposalID)
	stageExec(t, f, `CREATE TRIGGER fail_label_outbox BEFORE INSERT ON linear_outbox
		WHEN NEW.action='add_label' BEGIN SELECT RAISE(ABORT,'test label write failure'); END`)
	status, body := approvalRequest(t, f, "POST", "/api/decisions/"+decisionID+"/answer",
		`{"action":"approve","label":"route:b","basis":"`+proposalID+`"}`)
	if status != 500 {
		t.Fatalf("approve=%d %s", status, body)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM tasks WHERE id=? AND status='routing' AND project_id IS NULL AND harness_id IS NULL`, taskID); n != 1 {
		t.Fatal("outbox failure committed task destination or state")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE id=? AND status='open' AND answer IS NULL`, decisionID); n != 1 {
		t.Fatal("outbox failure committed the approval")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM executions WHERE task_id=?`, taskID) +
		stageCount(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND action='add_label'`, taskID); n != 0 {
		t.Fatal("outbox failure left partial rows")
	}
	requireProposal(t, f, proposalID, "proposed", "route:a", nil)
}

func TestRouteProposalFailedStillOpensDecision(t *testing.T) {
	for _, tc := range []struct{ name, body, errorClass string }{
		{"runner error", `{"label":null,"reason":null,"errorClass":"timeout"}`, "timeout"},
		{"missing authentication", `{"label":null,"reason":null,"errorClass":"config_error"}`, "config_error"},
		{"CLI failure", `{"label":null,"reason":null,"errorClass":"work_failed"}`, "work_failed"},
		{"label outside candidates", `{"label":"route:zzz","reason":"guess","errorClass":null}`, "contract_error"},
		{"reason too long", `{"label":"route:a","reason":"` + strings.Repeat("あ", 2001) + `","errorClass":null}`, "contract_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, proposalID, taskID := routeProposalFixture(t)
			status, body := proposalResult(t, f, token, proposalID, tc.body)
			if status != 200 {
				t.Fatalf("result=%d %s", status, body)
			}
			requireProposal(t, f, proposalID, "failed", nil, tc.errorClass)
			decisionID := routeDecisionOf(t, f, proposalID)
			requireDecisionOpenComment(t, f, taskID, decisionID)
			status, body = approvalRequest(t, f, "POST", "/api/decisions/"+decisionID+"/answer", `{"action":"approve","label":"route:a","basis":"`+proposalID+`"}`)
			if status != 200 {
				t.Fatalf("approve=%d %s", status, body)
			}
			if n := stageCount(t, f, `SELECT count(*) FROM tasks t JOIN executions e ON e.task_id=t.id
				WHERE t.id=? AND t.status='queued' AND t.project_id='project-a' AND e.seq=1 AND e.status='pending'`, taskID); n != 1 {
				t.Fatal("failed proposal approval did not queue")
			}
		})
	}
}

func TestRouteDecisionRejectCancelsTask(t *testing.T) {
	f, token, proposalID, taskID := routeProposalFixture(t)
	if status, body := proposalResult(t, f, token, proposalID, `{"label":"route:a","reason":"fits A","errorClass":null}`); status != 200 {
		t.Fatalf("result=%d %s", status, body)
	}
	decisionID := routeDecisionOf(t, f, proposalID)
	status, body := approvalRequest(t, f, "POST", "/api/decisions/"+decisionID+"/answer", `{"action":"reject"}`)
	if status != 200 {
		t.Fatalf("reject=%d %s", status, body)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM tasks WHERE id=? AND status='cancelled' AND project_id IS NULL`, taskID); n != 1 {
		t.Fatal("task not cancelled")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM executions WHERE task_id=?`, taskID); n != 0 {
		t.Fatal("reject created an Execution")
	}
	requireProposal(t, f, proposalID, "decided", "route:a", nil)
	if n := stageCount(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND action='add_label'`, taskID); n != 0 {
		t.Fatal("reject wrote a label")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND kind='cancelled' AND action='set_state'`, taskID); n != 1 {
		t.Fatal("reject did not move the Issue to Canceled")
	}
}

func TestRouteDecisionApproveNeedsRoutingTask(t *testing.T) {
	f, token, proposalID, taskID := routeProposalFixture(t)
	if status, body := proposalResult(t, f, token, proposalID, `{"label":"route:a","reason":"fits A","errorClass":null}`); status != 200 {
		t.Fatalf("result=%d %s", status, body)
	}
	decisionID := routeDecisionOf(t, f, proposalID)
	stageExec(t, f, `UPDATE tasks SET status='cancelled' WHERE id=?`, taskID)
	status, body := approvalRequest(t, f, "POST", "/api/decisions/"+decisionID+"/answer", `{"action":"approve","label":"route:a","basis":"`+proposalID+`"}`)
	assertAuthError(t, status, body, 409, Conflict)
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE id=? AND status='open' AND answer IS NULL`, decisionID); n != 1 {
		t.Fatal("conflicting answer changed decision")
	}
	requireProposal(t, f, proposalID, "proposed", "route:a", nil)
	if n := stageCount(t, f, `SELECT count(*) FROM executions WHERE task_id=?`, taskID) + stageCount(t, f, `SELECT count(*) FROM linear_outbox WHERE kind<>'decision_open'`); n != 0 {
		t.Fatal("conflicting answer wrote rows")
	}
}

func TestRouteProposalResultAfterCancelIsOnlyRecorded(t *testing.T) {
	f, token, proposalID, taskID := routeProposalFixture(t)
	stageExec(t, f, `UPDATE route_proposals SET status='cancelled' WHERE id=?`, proposalID)
	stageExec(t, f, `UPDATE tasks SET status='cancelled' WHERE id=?`, taskID)
	if status, body := proposalResult(t, f, token, proposalID, `{"label":"route:a","reason":"late","errorClass":null}`); status != 200 {
		t.Fatalf("result=%d %s", status, body)
	}
	requireProposal(t, f, proposalID, "cancelled", "route:a", nil)
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE task_id=?`, taskID); n != 0 {
		t.Fatal("late result opened a decision")
	}
}

func TestRouteProposalResultRejectsInvalidRequests(t *testing.T) {
	f, token, proposalID, _ := routeProposalFixture(t)
	for _, body := range []string{
		`{"label":"route:a","reason":"x"}`,
		`{"label":"route:a","reason":"x","errorClass":null,"extra":1}`,
		`{"label":null,"reason":"x","errorClass":null}`,
		`{"label":"route:a","reason":null,"errorClass":null}`,
		`{"label":"route:a","reason":null,"errorClass":"timeout"}`,
		`{"label":null,"reason":null,"errorClass":"interrupted"}`,
		`{"label":1,"reason":"x","errorClass":null}`,
	} {
		status, raw := proposalResult(t, f, token, proposalID, body)
		assertAuthError(t, status, raw, 400, ValidationFailed)
	}
	status, raw := proposalResult(t, f, token, ulid.Make().String(), `{"label":null,"reason":null,"errorClass":"timeout"}`)
	assertAuthError(t, status, raw, 404, NotFound)
	other, err := runnerauth.Register(context.Background(), f.db, "other-runner", "Other Runner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	status, raw = proposalResult(t, f, other, proposalID, `{"label":null,"reason":null,"errorClass":"timeout"}`)
	assertAuthError(t, status, raw, 403, Forbidden)
	requireProposal(t, f, proposalID, "running", nil, nil)
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions`); n != 0 {
		t.Fatal("rejected request opened a decision")
	}
	// A queued proposal has no Runner yet.
	stageExec(t, f, `UPDATE route_proposals SET status='queued', runner_id=NULL WHERE id=?`, proposalID)
	status, raw = proposalResult(t, f, token, proposalID, `{"label":null,"reason":null,"errorClass":"timeout"}`)
	assertAuthError(t, status, raw, 403, Forbidden)
}

// spec 15 3.6.4 (Q112): the approved label must be in routes at the time of
// the decision; otherwise 422 and nothing changes.
func TestRouteDecisionApproveRejectsLabelOutsideRoutes(t *testing.T) {
	f, token, proposalID, taskID := routeProposalFixture(t)
	if status, body := proposalResult(t, f, token, proposalID, `{"label":"route:a","reason":"fits A","errorClass":null}`); status != 200 {
		t.Fatalf("result=%d %s", status, body)
	}
	decisionID := routeDecisionOf(t, f, proposalID)
	// route:b was a candidate but is no longer in routes; route:zzz never was.
	stageExec(t, f, `DELETE FROM routes WHERE label='route:b'`)
	for _, label := range []string{"route:b", "route:zzz"} {
		status, body := approvalRequest(t, f, "POST", "/api/decisions/"+decisionID+"/answer",
			`{"action":"approve","label":"`+label+`","basis":"`+proposalID+`"}`)
		assertAuthError(t, status, body, 422, PreconditionFailed)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE id=? AND status='open' AND answer IS NULL`, decisionID); n != 1 {
		t.Fatal("rejected approval answered the decision")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM tasks WHERE id=? AND status='routing' AND project_id IS NULL`, taskID); n != 1 {
		t.Fatal("rejected approval changed the task")
	}
	requireProposal(t, f, proposalID, "proposed", "route:a", nil)
	if n := stageCount(t, f, `SELECT count(*) FROM executions WHERE task_id=?`, taskID) +
		stageCount(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND kind<>'decision_open'`, taskID); n != 0 {
		t.Fatal("rejected approval wrote rows")
	}
}

// spec 15 3.6.4 step 2: with routing.proposer.model null, claim never hands
// the queued proposal to the Runner; it fails with error_class NULL and a
// route decision without a proposal is opened.
func TestClaimFailsProposalWithoutProposerModel(t *testing.T) {
	f, token, profiles := claimFixture(t)
	enableDirectRouting(t, f, profiles[0].ID, "claim-runner")
	stageExec(t, f, `UPDATE settings SET value='{"microHarness":"claude_code","model":null}' WHERE key='routing.proposer'`)
	insertRouteProposal(t, f, "proposal-1", "task-901", "2026-12-01T00:00:00Z")
	stageExec(t, f, `INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id, task_id, attach_seq, created_at)
		VALUES ('link-1', 'issue-1', 'ENG-1', 'https://linear.app/x/issue/ENG-1', 'team-a', 'task-901', 1, 't')`)
	status, body := requestClaim(t, f, token)
	if kind := claimJobKind(t, status, body); kind != "" {
		t.Fatalf("claim handed %q: %s", kind, body)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM route_proposals WHERE id='proposal-1' AND status='failed'
		AND error_class IS NULL AND runner_id IS NULL AND proposed_label IS NULL AND finished_at IS NOT NULL`); n != 1 {
		t.Fatal("proposal not failed without a Runner")
	}
	decisionID := routeDecisionOf(t, f, "proposal-1")
	requireDecisionOpenComment(t, f, "task-901", decisionID)
	if n := stageCount(t, f, `SELECT count(*) FROM tasks WHERE id='task-901' AND status='routing'`) -
		stageCount(t, f, `SELECT count(*) FROM executions WHERE task_id='task-901'`); n != 1 {
		t.Fatal("task left routing or got an Execution")
	}
}
