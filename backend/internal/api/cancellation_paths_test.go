package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/oklog/ulid/v2"
)

// workspaceOf returns the status of the Execution's pinned workspace, the
// spec 04 3.8 "the workspace is not deleted" check of every path.
func workspaceOf(t *testing.T, f *authFixture, executionID string) (string, bool) {
	t.Helper()
	var status string
	var cleaned bool
	if err := f.db.QueryRow(`SELECT w.status, w.cleaned_at IS NOT NULL FROM task_workspaces w
		JOIN executions e ON e.workspace_id = w.id WHERE e.id = ?`, executionID).Scan(&status, &cleaned); err != nil {
		t.Fatalf("workspace of %s: %v", executionID, err)
	}
	return status, cleaned
}

func requireWorkspaceKept(t *testing.T, f *authFixture, executionID, before string) {
	t.Helper()
	status, cleaned := workspaceOf(t, f, executionID)
	if status != before || status == "cleaned" || cleaned {
		t.Fatalf("workspace = %s cleaned=%v; want kept as %s", status, cleaned, before)
	}
}

func decisionState(t *testing.T, f *authFixture, id string) (string, string) {
	t.Helper()
	var status, reason string
	if err := f.db.QueryRow(`SELECT status, coalesce(withdrawn_reason, '') FROM owner_decisions WHERE id = ?`, id).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	return status, reason
}

// Spec 04 3.8 blocked row: the open decision is withdrawn (cancelled) and the
// Execution and task become cancelled, for every cause but publish_unknown.
func TestCancelBlockedWithdrawsDecisionAndKeepsWorkspace(t *testing.T) {
	for _, d := range []struct{ kind, cause string }{
		{"blocked", "check_blocked"}, {"blocked", "context_conflict"}, {"blocked", "workspace_missing"}, {"question", "question"},
	} {
		t.Run(d.cause, func(t *testing.T) {
			f, _, id, _ := resultFixture(t, "checking")
			blockExecution(t, f, id, "checking")
			decisionID := seedDecision(t, f, id, d.kind, d.cause)
			workspace, _ := workspaceOf(t, f, id)
			before := cancellationState(t, f, id)
			status, body := requestCancellation(t, f, executionTaskID(t, f, id))
			if status != 204 {
				t.Fatalf("cancel blocked=%d %s", status, body)
			}
			requireCancelled(t, f, id, before)
			if status, reason := decisionState(t, f, decisionID); status != "withdrawn" || reason != "cancelled" {
				t.Fatalf("decision = %s/%s; want withdrawn/cancelled", status, reason)
			}
			requireWorkspaceKept(t, f, id, workspace)
		})
	}
}

// Spec 04 3.8 blocked row, 02 5.2: a publish whose outcome is unknown ends
// failed (publish_unknown), not cancelled.
func TestCancelPublishUnknownDecisionFailsExecution(t *testing.T) {
	f, _, id, _ := resultFixture(t, "publishing")
	blockExecution(t, f, id, "publishing")
	decisionID := seedDecision(t, f, id, "blocked", "publish_unknown")
	workspace, _ := workspaceOf(t, f, id)
	before := cancellationState(t, f, id)
	status, body := requestCancellation(t, f, executionTaskID(t, f, id))
	if status != 204 || len(body) != 0 {
		t.Fatalf("cancel publish_unknown=%d %s", status, body)
	}
	after := cancellationState(t, f, id)
	if after.Execution != "failed" || after.Task != "failed" || after.ErrorClass != "publish_unknown" ||
		after.Requested.Valid || !after.Finished.Valid || after.Events != before.Events+1 || after.Audits != before.Audits+1 {
		t.Fatalf("publish_unknown cancel before=%+v after=%+v", before, after)
	}
	var from, to, reason string
	if err := f.db.QueryRow(`SELECT json_extract(payload, '$.from'), json_extract(payload, '$.to'),
		json_extract(payload, '$.reason') FROM execution_events WHERE execution_id = ? ORDER BY id DESC LIMIT 1`, id).
		Scan(&from, &to, &reason); err != nil || from != "blocked" || to != "failed" || reason != "task_cancelled" {
		t.Fatalf("cancel event: %s -> %s reason=%s err=%v", from, to, reason, err)
	}
	assertCancellationAudit(t, f, id, "blocked", "failed", false)
	if status, reason := decisionState(t, f, decisionID); status != "withdrawn" || reason != "cancelled" {
		t.Fatalf("decision = %s/%s; want withdrawn/cancelled", status, reason)
	}
	requireWorkspaceKept(t, f, id, workspace)
	status, body = requestCancellation(t, f, executionTaskID(t, f, id))
	assertAuthError(t, status, body, 409, Conflict)
}

// Spec 04 3.8 waiting_approval row: every unused approval is invalidated
// (cancelled) and its decision withdrawn; the workspace stays.
func TestCancelWaitingApprovalInvalidatesAllApprovalsAndKeepsWorkspace(t *testing.T) {
	f, _, id, _ := resultFixture(t, "publishing")
	setApprovalState(t, f, id, "waiting_approval")
	insertApproval(t, f, "approval-approved", id, "approved")
	stageExec(t, f, `UPDATE approvals SET operation='deploy_production' WHERE id='approval-approved'`)
	stageExec(t, f, `UPDATE owner_decisions SET status='answered' WHERE id='decision-approval-approved'`)
	insertApproval(t, f, "approval-pending", id, "pending")
	workspace, _ := workspaceOf(t, f, id)
	before := cancellationState(t, f, id)
	status, body := requestCancellation(t, f, executionTaskID(t, f, id))
	if status != 204 {
		t.Fatalf("cancel=%d %s", status, body)
	}
	requireCancelled(t, f, id, before)
	for _, approval := range []string{"approval-approved", "approval-pending"} {
		if status, reason := approvalStatus(t, f, approval); status != "invalidated" || reason != "cancelled" {
			t.Fatalf("%s = %s/%s; want invalidated/cancelled", approval, status, reason)
		}
	}
	if status, reason := decisionState(t, f, "decision-approval-pending"); status != "withdrawn" || reason != "cancelled" {
		t.Fatalf("pending decision = %s/%s", status, reason)
	}
	if status, _ := decisionState(t, f, "decision-approval-approved"); status != "answered" {
		t.Fatalf("answered decision changed to %s", status)
	}
	requireWorkspaceKept(t, f, id, workspace)
}

// Spec 04 3.8 queued row: a resuming Execution's approved but unused
// approval is invalidated (cancelled); a consumed one is left alone.
func TestCancelQueuedResumeInvalidatesApprovedUnusedApproval(t *testing.T) {
	f, token, id, _ := resultFixture(t, "publishing")
	setApprovalState(t, f, id, "waiting_approval")
	insertApproval(t, f, "approval-approved", id, "approved")
	insertApproval(t, f, "approval-consumed", id, "consumed")
	stageExec(t, f, `UPDATE owner_decisions SET status='answered' WHERE execution_id=?`, id)
	stageExec(t, f, `UPDATE executions SET status='pending', resume_stage='publish' WHERE id=?`, id)
	stageExec(t, f, `UPDATE tasks SET status='queued' WHERE id=(SELECT task_id FROM executions WHERE id=?)`, id)
	workspace, _ := workspaceOf(t, f, id)
	before := cancellationState(t, f, id)
	status, body := requestCancellation(t, f, executionTaskID(t, f, id))
	if status != 204 {
		t.Fatalf("cancel=%d %s", status, body)
	}
	requireCancelled(t, f, id, before)
	if status, reason := approvalStatus(t, f, "approval-approved"); status != "invalidated" || reason != "cancelled" {
		t.Fatalf("approved approval = %s/%s; want invalidated/cancelled", status, reason)
	}
	if status, reason := approvalStatus(t, f, "approval-consumed"); status != "consumed" || reason != "" {
		t.Fatalf("consumed approval changed to %s/%s", status, reason)
	}
	requireWorkspaceKept(t, f, id, workspace)
	status, body = requestClaim(t, f, token)
	requireNoJob(t, status, body)
}

// Spec 04 3.8 running row: the workspace stays while the Runner is asked to
// stop and after it reports cancelled.
func TestCancelRunningKeepsWorkspace(t *testing.T) {
	f, token, id, _ := resultFixture(t, "working")
	workspace, _ := workspaceOf(t, f, id)
	if status, body := requestCancellation(t, f, executionTaskID(t, f, id)); status != 204 {
		t.Fatalf("cancel=%d %s", status, body)
	}
	status, body := postHeartbeat(t, f, token, "claim-runner", heartbeatInput(id))
	requireHeartbeat(t, status, body, id)
	requireWorkspaceKept(t, f, id, workspace)
	if status, body := postResult(t, f, token, id, resultBody("working", "cancelled")); status != 204 {
		t.Fatalf("runner cancelled=%d %s", status, body)
	}
	requireWorkspaceKept(t, f, id, workspace)
}

// routingFixture is routeProposalFixture with an owner API task ID
// (task-N), so the owner can cancel it.
func routingFixture(t *testing.T) (*authFixture, string, string, string) {
	t.Helper()
	f, token, _ := claimFixture(t)
	proposalID, taskID := ulid.Make().String(), "task-910001"
	insertRouteProposal(t, f, proposalID, taskID, "2026-11-01T00:00:00Z")
	stageExec(t, f, `UPDATE route_proposals SET status='running', runner_id='claim-runner',
		candidates='[{"label":"route:a","kind":"project","id":"project-a","description":"A"}]' WHERE id=?`, proposalID)
	stageExec(t, f, `DELETE FROM routes`)
	stageExec(t, f, `INSERT INTO routes VALUES ('route:a','project-a',NULL,'t','t')`)
	stageExec(t, f, `INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id, task_id, attach_seq, created_at)
		VALUES (?, 'issue-1', 'ENG-1', 'https://linear.app/x/issue/ENG-1', 'team-a', ?, 1, 't')`, ulid.Make().String(), taskID)
	return f, token, proposalID, taskID
}

func heartbeatProposals(t *testing.T, f *authFixture, token string, proposals ...string) (int, []string) {
	t.Helper()
	input := heartbeatInput()
	if proposals != nil {
		input["runningRouteProposalIds"] = proposals
	}
	status, body := postHeartbeat(t, f, token, "claim-runner", input)
	var reply struct {
		Executions []string `json:"cancelExecutionIds"`
		Proposals  []string `json:"cancelRouteProposalIds"`
	}
	if status == 200 {
		if err := json.Unmarshal(body, &reply); err != nil || reply.Executions == nil || reply.Proposals == nil {
			t.Fatalf("heartbeat body %s err=%v", body, err)
		}
	}
	return status, reply.Proposals
}

// Spec 04 3.8 routing row and 3.7 (Q133): the route decision is withdrawn,
// a queued proposal is cancelled and the task becomes cancelled.
func TestCancelRoutingWithdrawsDecisionAndCancelsQueuedProposal(t *testing.T) {
	f, token, proposalID, taskID := routingFixture(t)
	if status, body := proposalResult(t, f, token, proposalID, `{"label":"route:a","reason":"fits A","errorClass":null}`); status != 200 {
		t.Fatalf("result=%d %s", status, body)
	}
	decisionID := routeDecisionOf(t, f, proposalID)
	// A second, still queued proposal of the same task.
	stageExec(t, f, `INSERT INTO route_proposals (id, task_id, trigger, seen_labels, candidates, status, created_at)
		VALUES ('01JQUEUEDPROPOSAL000000000', ?, 'no_label', '[]', '[]', 'queued', 't')`, taskID)
	status, body := requestCancellation(t, f, taskID)
	if status != 204 {
		t.Fatalf("cancel routing=%d %s", status, body)
	}
	if got := stageCount(t, f, `SELECT count(*) FROM tasks WHERE id=? AND status='cancelled'`, taskID); got != 1 {
		t.Fatal("routing task not cancelled")
	}
	if status, reason := decisionState(t, f, decisionID); status != "withdrawn" || reason != "cancelled" {
		t.Fatalf("route decision = %s/%s; want withdrawn/cancelled", status, reason)
	}
	requireProposal(t, f, "01JQUEUEDPROPOSAL000000000", "cancelled", nil, nil)
	requireProposal(t, f, proposalID, "proposed", "route:a", nil)
	var detail string
	if err := f.db.QueryRow(`SELECT detail FROM audit_logs WHERE action='task.cancel' AND target_id=?`, taskID).Scan(&detail); err != nil ||
		detail != `{"cancelRequested":false,"from":"routing","to":"cancelled"}` {
		t.Fatalf("routing cancel audit %s err=%v", detail, err)
	}
	if status, ids := heartbeatProposals(t, f, token); status != 200 || len(ids) != 0 {
		t.Fatalf("heartbeat for a finished proposal = %d %v", status, ids)
	}
}

// Spec 04 3.7/3.8 (Q133): a running proposal keeps its slot until the Runner
// stops it; heartbeat tells the Runner, and its result makes it cancelled.
func TestCancelRoutingStopsRunningProposalThroughHeartbeat(t *testing.T) {
	f, token, proposalID, taskID := routingFixture(t)
	if status, ids := heartbeatProposals(t, f, token, proposalID); status != 200 || len(ids) != 0 {
		t.Fatalf("heartbeat before cancel = %d %v", status, ids)
	}
	if status, body := requestCancellation(t, f, taskID); status != 204 {
		t.Fatalf("cancel routing=%d %s", status, body)
	}
	requireProposal(t, f, proposalID, "running", nil, nil)
	for i := 0; i < 2; i++ {
		// Not limited to the reported list, and repeated until the result.
		status, ids := heartbeatProposals(t, f, token)
		if status != 200 || !reflect.DeepEqual(ids, []string{proposalID}) {
			t.Fatalf("heartbeat after cancel = %d %v", status, ids)
		}
	}
	if status, body := proposalResult(t, f, token, proposalID, `{"label":null,"reason":null,"errorClass":"timeout"}`); status != 200 {
		t.Fatalf("result=%d %s", status, body)
	}
	requireProposal(t, f, proposalID, "cancelled", nil, "timeout")
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE task_id=?`, taskID); n != 0 {
		t.Fatal("stopped proposal opened a decision")
	}
	if status, ids := heartbeatProposals(t, f, token, proposalID); status != 200 || len(ids) != 0 {
		t.Fatalf("heartbeat after result = %d %v", status, ids)
	}
}

// spec 05 5: a reported proposal must belong to the Runner.
func TestHeartbeatRejectsForeignOrUnknownProposal(t *testing.T) {
	f, token, proposalID, _ := routeProposalFixture(t)
	stageExec(t, f, `UPDATE route_proposals SET runner_id=NULL, status='queued' WHERE id=?`, proposalID)
	status, _ := heartbeatProposals(t, f, token, proposalID)
	if status != 403 {
		t.Fatalf("foreign proposal heartbeat = %d; want 403", status)
	}
	status, _ = heartbeatProposals(t, f, token, "not-a-ulid")
	if status != 400 {
		t.Fatalf("invalid proposal ID heartbeat = %d; want 400", status)
	}
}
