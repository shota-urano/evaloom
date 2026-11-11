package api

import (
	"reflect"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// claimForResume claims a new task on project-a with profiles[1] pinned,
// then returns the Execution to pending with resume_stage as an owner
// decision would (spec 04 3.6), and changes the Project afterwards.
func claimForResume(t *testing.T, stage string, setup func(f *authFixture, job store.ExecutionJob)) (*authFixture, string, store.ExecutionJob, executionPin) {
	t.Helper()
	f, token, profiles := claimFixture(t)
	createClaimTask(t, f, "project-a", "resume-"+stage)
	status, body := postAssignment(t, f, "project-a", "developer", "harness-a", profiles[1].Version)
	requireAssignment(t, status, body, 200)
	status, body = requestClaim(t, f, token)
	first := requireClaim(t, status, body)
	pin := readExecutionPin(t, f, first.Execution.ID)
	setup(f, first)
	execSQL(t, f, `UPDATE executions SET status = 'pending', resume_stage = ? WHERE id = ?`, stage, first.Execution.ID)
	execSQL(t, f, `UPDATE tasks SET status = 'queued' WHERE id = ?`, first.Execution.TaskID)
	// The Project's version and params change while the Execution waits.
	status, body = postAssignment(t, f, "project-a", "developer", "harness-a", profiles[0].Version)
	requireAssignment(t, status, body, 200)
	execSQL(t, f, `UPDATE projects SET params = '{"changed":true}' WHERE id = 'project-a'`)
	return f, token, first, pin
}

func execSQL(t *testing.T, f *authFixture, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func insertStageRun(t *testing.T, f *authFixture, executionID, stage, status string) {
	t.Helper()
	execSQL(t, f, `INSERT INTO stage_runs (id, execution_id, stage, attempt, status, started_at, finished_at)
		VALUES (?, ?, ?, 1, ?, '2026-11-11T00:00:00Z', '2026-11-11T00:01:00Z')`,
		executionID+"-"+stage, executionID, stage, status)
}

func requireResumePinned(t *testing.T, f *authFixture, first, job store.ExecutionJob, pin executionPin) {
	t.Helper()
	if job.Execution.ID != first.Execution.ID || !reflect.DeepEqual(job.Profile, first.Profile) ||
		string(job.Params) != string(first.Params) || job.Workspace.ID != first.Workspace.ID ||
		job.Workspace.Path != first.Workspace.Path || job.Workspace.Created {
		t.Fatalf("resumed claim changed pins: job=%+v first=%+v", job, first)
	}
	after := readExecutionPin(t, f, job.Execution.ID)
	after.ClaimedAt, pin.ClaimedAt = "", ""
	if after != pin {
		t.Fatalf("resumed claim rewrote the Execution: %+v want %+v", after, pin)
	}
	var workspaces int
	if err := f.db.QueryRow(`SELECT count(*) FROM task_workspaces WHERE task_id = ?`, job.Execution.TaskID).Scan(&workspaces); err != nil || workspaces != 1 {
		t.Fatalf("workspaces=%d err=%v", workspaces, err)
	}
	var execStatus, taskStatus string
	if err := f.db.QueryRow(`SELECT e.status, t.status FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`,
		job.Execution.ID).Scan(&execStatus, &taskStatus); err != nil || execStatus != "claimed" || taskStatus != "running" {
		t.Fatalf("execution=%s task=%s err=%v", execStatus, taskStatus, err)
	}
}

// spec 04 9 (resume claim), 3.3 step 5, 3.3.1: a publish resume keeps the
// pinned version and workspace and passes approved and waived operations.
func TestClaimResumePublishKeepsPinsAndPassesApprovals(t *testing.T) {
	f, token, first, pin := claimForResume(t, "publish", func(f *authFixture, job store.ExecutionJob) {
		id, task := job.Execution.ID, job.Execution.TaskID
		insertStageRun(t, f, id, "work", "succeeded")
		insertStageRun(t, f, id, "check", "needs_approval")
		execSQL(t, f, `UPDATE executions SET content_id = 'content-1' WHERE id = ?`, id)
		execSQL(t, f, `INSERT INTO owner_decisions (id, kind, task_id, execution_id, subject_id, basis, detail, status, answer, answered_at, created_at)
			VALUES ('decision-approval', 'approval', ?, ?, 'approval-1', 'content-1', '{}', 'answered', '{"action":"approve","contentId":"content-1"}',
			'2026-11-11T00:02:00Z', '2026-11-11T00:01:00Z')`, task, id)
		execSQL(t, f, `INSERT INTO approvals (id, task_id, execution_id, decision_id, requested_stage, operation, target, content_id,
			params_hash, status, decided_at, expires_at, created_at)
			VALUES ('approval-1', ?, ?, 'decision-approval', 'check', 'merge_pull_request', '{}', 'content-1', 'hash', 'approved',
			'2026-11-11T00:02:00Z', '2999-01-01T00:00:00Z', '2026-11-11T00:01:00Z')`, task, id)
		for _, payload := range []string{`{"operation":"deploy_production","contentId":"content-1"}`,
			`{"operation":"deploy_production","contentId":"content-1"}`} {
			if _, err := store.AppendExecutionEvent(t.Context(), f.db, id, model.EventApprovalWaived, []byte(payload)); err != nil {
				t.Fatal(err)
			}
		}
	})
	status, body := requestClaim(t, f, token)
	job := requireClaim(t, status, body)
	requireResumePinned(t, f, first, job, pin)
	if job.Execution.ResumeStage == nil || *job.Execution.ResumeStage != model.StagePublish || job.Resume == nil {
		t.Fatalf("resume missing: %s", body)
	}
	r := job.Resume
	approvalID, content := "approval-1", "content-1"
	want := []struct{ id, operation, status string }{{approvalID, "merge_pull_request", "approved"}, {"", "deploy_production", "waived"}}
	if r.Stage != model.StagePublish || r.CheckedContentID == nil || *r.CheckedContentID != content || r.Answer != nil ||
		r.WorkSessionID != nil || len(r.Approvals) != len(want) ||
		!reflect.DeepEqual(r.CompletedStages, []model.Stage{model.StageWork, model.StageCheck}) {
		t.Fatalf("resume=%s", body)
	}
	for i, w := range want {
		a := r.Approvals[i]
		gotID := ""
		if a.ID != nil {
			gotID = *a.ID
		}
		if gotID != w.id || a.Operation != w.operation || a.Status != w.status || a.ContentID == nil || *a.ContentID != content {
			t.Fatalf("approvals[%d]=%+v body=%s", i, a, body)
		}
	}
}

// spec 04 9 (resume claim), 3.3.1: a work resume passes the owner's answer
// and the work session to reopen.
func TestClaimResumeWorkPassesAnswerAndSession(t *testing.T) {
	f, token, first, pin := claimForResume(t, "work", func(f *authFixture, job store.ExecutionJob) {
		id, task := job.Execution.ID, job.Execution.TaskID
		insertStageRun(t, f, id, "work", "question")
		execSQL(t, f, `UPDATE executions SET work_session_id = 'session-1' WHERE id = ?`, id)
		execSQL(t, f, `INSERT INTO owner_decisions (id, kind, task_id, execution_id, cause, detail, status, answer, answered_at, created_at)
			VALUES ('decision-question', 'question', ?, ?, 'question', '{}', 'answered', '{"action":"answer","text":"Use plan B","option":"B"}',
			'2026-11-11T00:02:00Z', '2026-11-11T00:01:00Z')`, task, id)
	})
	status, body := requestClaim(t, f, token)
	job := requireClaim(t, status, body)
	requireResumePinned(t, f, first, job, pin)
	r := job.Resume
	if r == nil || r.Stage != model.StageWork || r.WorkSessionID == nil || *r.WorkSessionID != "session-1" ||
		r.Answer == nil || r.Answer.DecisionID != "decision-question" || r.Answer.Text != "Use plan B" ||
		r.Answer.Option == nil || *r.Answer.Option != "B" || r.CheckedContentID != nil || len(r.Approvals) != 0 ||
		len(r.CompletedStages) != 0 {
		t.Fatalf("resume=%s", body)
	}
}

// spec 04 9 (resume claim), 3.3 step 5, 3.5: a resume whose workspace was
// cleaned becomes blocked (workspace_missing) with an open decision, and the
// next candidate is examined.
func TestClaimResumeCleanedWorkspaceBlocks(t *testing.T) {
	f, token, first, _ := claimForResume(t, "work", func(f *authFixture, job store.ExecutionJob) {
		execSQL(t, f, `UPDATE task_workspaces SET status = 'cleaned', cleaned_at = '2026-11-11T00:03:00Z' WHERE id = ?`, job.Workspace.ID)
	})
	next := createClaimTask(t, f, "project-b", "after-cleaned")
	setExecutionCreatedAt(t, f, model.Task{ID: first.Execution.TaskID}, next)
	status, body := requestClaim(t, f, token)
	job := requireClaim(t, status, body)
	if job.Execution.TaskID != next.ID {
		t.Fatalf("claimed %s; want the next candidate %s", job.Execution.TaskID, next.ID)
	}
	var execStatus, taskStatus, resume string
	if err := f.db.QueryRow(`SELECT e.status, t.status, e.resume_stage FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`,
		first.Execution.ID).Scan(&execStatus, &taskStatus, &resume); err != nil ||
		execStatus != "blocked" || taskStatus != "blocked" || resume != "work" {
		t.Fatalf("execution=%s task=%s resume=%s err=%v", execStatus, taskStatus, resume, err)
	}
	var decisionID, opened string
	if err := f.db.QueryRow(`SELECT id FROM owner_decisions WHERE execution_id = ? AND kind = 'blocked'
		AND cause = 'workspace_missing' AND status = 'open' AND basis IS NULL`, first.Execution.ID).Scan(&decisionID); err != nil {
		t.Fatalf("decision err=%v", err)
	}
	if err := f.db.QueryRow(`SELECT json_extract(payload, '$.decisionId') FROM execution_events
		WHERE execution_id = ? AND type = 'decision_opened'`, first.Execution.ID).Scan(&opened); err != nil || opened != decisionID {
		t.Fatalf("decision_opened=%s err=%v", opened, err)
	}
	var notifications int
	if err := f.db.QueryRow(`SELECT count(*) FROM notification_requests WHERE execution_id = ? AND status = 'blocked'`,
		first.Execution.ID).Scan(&notifications); err != nil || notifications != 1 {
		t.Fatalf("notifications=%d err=%v", notifications, err)
	}
	var claimedWorkspace string
	if err := f.db.QueryRow(`SELECT coalesce(workspace_id, '') FROM executions WHERE id = ?`, first.Execution.ID).
		Scan(&claimedWorkspace); err != nil || claimedWorkspace != first.Workspace.ID {
		t.Fatalf("workspace pin=%s err=%v", claimedWorkspace, err)
	}
}
