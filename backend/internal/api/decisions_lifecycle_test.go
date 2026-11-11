package api

import (
	"testing"
)

// Spec 04 3.6: each answer of 02 5.6 moves the Execution as the table says.

func openDecisionID(t *testing.T, f *authFixture, executionID, kind string) string {
	t.Helper()
	var id string
	if err := f.db.QueryRow(`SELECT id FROM owner_decisions WHERE execution_id = ? AND kind = ? AND status = 'open' ORDER BY id LIMIT 1`,
		executionID, kind).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func answer(t *testing.T, f *authFixture, decisionID, body string, want int) {
	t.Helper()
	status, out := approvalRequest(t, f, "POST", "/api/decisions/"+decisionID+"/answer", body)
	if status != want {
		t.Fatalf("answer %s = %d %s; want %d", body, status, out, want)
	}
}

func requireResume(t *testing.T, f *authFixture, id, stage string) {
	t.Helper()
	s := readState(t, f, id)
	if s.Execution != "pending" || s.Task != "queued" || s.ResumeStage.String != stage {
		t.Fatalf("state=%+v; want pending/queued resume=%s", s, stage)
	}
}

func requireAnswered(t *testing.T, f *authFixture, decisionID string) {
	t.Helper()
	if n := count(t, f, `SELECT count(*) FROM owner_decisions WHERE id = ? AND status = 'answered' AND answered_at IS NOT NULL`, decisionID); n != 1 {
		t.Fatal("decision not answered")
	}
	if n := count(t, f, `SELECT count(*) FROM audit_logs WHERE action = 'decision.answer' AND actor = 'owner'
		AND target_type = 'owner_decision' AND target_id = ?`, decisionID); n != 1 {
		t.Fatalf("decision audit=%d", n)
	}
}

func questionFixture(t *testing.T) (*authFixture, string) {
	t.Helper()
	f, token, id, _ := reportFixture(t)
	attempt := startStage(t, f, token, id, "work")
	input := finished("work", attempt, nil)
	input["work"] = map[string]any{"sessionId": "session-9", "summaryArtifactId": nil,
		"question": map[string]any{"question": "Keep or remove?", "options": []string{"keep", "remove"}}}
	finishStage(t, f, token, id, input)
	requireState(t, f, id, "blocked", "blocked", "")
	return f, id
}

func checkBlockedFixture(t *testing.T, requests ...string) (*authFixture, string) {
	t.Helper()
	f, token, id, _ := reportFixture(t)
	approvalDeclarations(t, f, id)
	execSQL(t, f, `UPDATE projects SET approval_waivers = '["deploy"]' WHERE id = 'project-a'`)
	attempt := runTo(t, f, token, id, "check")
	var list []any
	for _, op := range requests {
		list = append(list, stageApprovalRequest(op))
	}
	extra := map[string]any{}
	if len(list) > 0 {
		extra["approvalRequests"] = list
	}
	finishStage(t, f, token, id, finished("check", attempt, programResult("check", "blocked", extra)))
	requireState(t, f, id, "blocked", "blocked", "")
	return f, id
}

func publishUnknownFixture(t *testing.T) (*authFixture, string) {
	t.Helper()
	f, token, id, _ := reportFixture(t)
	attempt := runTo(t, f, token, id, "publish")
	finishStage(t, f, token, id, finished("publish", attempt, programResult("publish", "unknown", map[string]any{"errorClass": "publish_unknown"})))
	requireState(t, f, id, "blocked", "blocked", "")
	return f, id
}

func TestDecisionAnswerQuestion(t *testing.T) {
	t.Run("answer", func(t *testing.T) {
		f, id := questionFixture(t)
		decision := openDecisionID(t, f, id, "question")
		answer(t, f, decision, `{"action":"answer","text":"keep it","option":"keep"}`, 200)
		requireResume(t, f, id, "work")
		requireAnswered(t, f, decision)
		if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = 'status_changed'
			AND json_extract(payload, '$.from') = 'blocked' AND json_extract(payload, '$.to') = 'pending'`, id); n != 1 {
			t.Fatalf("status events=%d", n)
		}
		if n := count(t, f, `SELECT count(*) FROM executions WHERE task_id = (SELECT task_id FROM executions WHERE id = ?)`, id); n != 1 {
			t.Fatal("resume created a new Execution")
		}
		// An answered decision is 409 and changes nothing.
		answer(t, f, decision, `{"action":"abort"}`, 409)
		requireResume(t, f, id, "work")
	})
	t.Run("abort", func(t *testing.T) {
		f, id := questionFixture(t)
		decision := openDecisionID(t, f, id, "question")
		answer(t, f, decision, `{"action":"abort"}`, 200)
		requireState(t, f, id, "cancelled", "cancelled", "cancelled")
		requireAnswered(t, f, decision)
	})
}

func TestDecisionAnswerPublishUnknown(t *testing.T) {
	t.Run("continue", func(t *testing.T) {
		f, id := publishUnknownFixture(t)
		decision := openDecisionID(t, f, id, "blocked")
		answer(t, f, decision, `{"action":"continue"}`, 200)
		requireResume(t, f, id, "publish")
		requireAnswered(t, f, decision)
	})
	t.Run("abort", func(t *testing.T) {
		f, id := publishUnknownFixture(t)
		decision := openDecisionID(t, f, id, "blocked")
		answer(t, f, decision, `{"action":"abort"}`, 200)
		requireState(t, f, id, "failed", "failed", "publish_unknown")
		requireAnswered(t, f, decision)
	})
}

func TestDecisionAnswerCheckBlocked(t *testing.T) {
	t.Run("continue_without_requests", func(t *testing.T) {
		f, id := checkBlockedFixture(t)
		decision := openDecisionID(t, f, id, "blocked")
		answer(t, f, decision, `{"action":"continue","contentId":"content-1"}`, 200)
		requireResume(t, f, id, "publish")
		requireAnswered(t, f, decision)
	})
	t.Run("continue_only_waived", func(t *testing.T) {
		f, id := checkBlockedFixture(t, "deploy")
		answer(t, f, openDecisionID(t, f, id, "blocked"), `{"action":"continue","contentId":"content-1"}`, 200)
		requireResume(t, f, id, "publish")
		if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = 'approval_waived'`, id); n != 1 {
			t.Fatalf("waived events=%d", n)
		}
		if n := count(t, f, `SELECT count(*) FROM approvals WHERE execution_id = ?`, id); n != 0 {
			t.Fatal("waived operation created an approval")
		}
	})
	t.Run("continue_with_requests", func(t *testing.T) {
		f, id := checkBlockedFixture(t, "merge_pull_request", "deploy")
		decision := openDecisionID(t, f, id, "blocked")
		answer(t, f, decision, `{"action":"continue","contentId":"content-1"}`, 200)
		requireState(t, f, id, "waiting_approval", "waiting_approval", "")
		requireAnswered(t, f, decision)
		if n := count(t, f, `SELECT count(*) FROM approvals WHERE execution_id = ? AND operation = 'merge_pull_request'
			AND status = 'pending' AND requested_stage = 'check' AND content_id = 'content-1'`, id); n != 1 {
			t.Fatalf("approvals=%d", n)
		}
		if n := count(t, f, `SELECT count(*) FROM approvals WHERE execution_id = ?`, id); n != 1 {
			t.Fatalf("all approvals=%d", n)
		}
		if n := count(t, f, `SELECT count(*) FROM owner_decisions WHERE execution_id = ? AND kind = 'approval' AND status = 'open'
			AND basis = 'content-1'`, id); n != 1 {
			t.Fatalf("approval decisions=%d", n)
		}
		if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id = ? AND type = 'approval_waived'`, id); n != 1 {
			t.Fatalf("waived events=%d", n)
		}
		// The approval then resumes publish as usual.
		answer(t, f, openDecisionID(t, f, id, "approval"), `{"action":"approve","contentId":"content-1"}`, 200)
		requireResume(t, f, id, "publish")
	})
	t.Run("abort", func(t *testing.T) {
		f, id := checkBlockedFixture(t, "merge_pull_request")
		decision := openDecisionID(t, f, id, "blocked")
		answer(t, f, decision, `{"action":"abort","contentId":"content-1"}`, 200)
		requireState(t, f, id, "cancelled", "cancelled", "cancelled")
		requireAnswered(t, f, decision)
		if n := count(t, f, `SELECT count(*) FROM approvals WHERE execution_id = ?`, id); n != 0 {
			t.Fatal("abort created an approval")
		}
	})
}

func TestDecisionAnswerAbortOnlyCauses(t *testing.T) {
	for _, cause := range []string{"context_conflict", "workspace_missing"} {
		t.Run(cause, func(t *testing.T) {
			f, id := checkBlockedFixture(t)
			decision := openDecisionID(t, f, id, "blocked")
			execSQL(t, f, `UPDATE owner_decisions SET cause = ?, basis = NULL WHERE id = ?`, cause, decision)
			answer(t, f, decision, `{"action":"continue"}`, 400)
			requireState(t, f, id, "blocked", "blocked", "")
			answer(t, f, decision, `{"action":"abort"}`, 200)
			requireState(t, f, id, "cancelled", "cancelled", "cancelled")
			requireAnswered(t, f, decision)
		})
	}
}

func twoApprovalFixture(t *testing.T) (*authFixture, string, []string) {
	t.Helper()
	f, token, id, _ := reportFixture(t)
	approvalDeclarations(t, f, id)
	attempt := runTo(t, f, token, id, "check")
	finishStage(t, f, token, id, finished("check", attempt, programResult("check", "needs_approval", map[string]any{
		"approvalRequests": []any{stageApprovalRequest("merge_pull_request"), stageApprovalRequest("deploy")},
	})))
	requireState(t, f, id, "waiting_approval", "waiting_approval", "")
	rows, err := f.db.Query(`SELECT id FROM owner_decisions WHERE execution_id = ? AND kind = 'approval' AND status = 'open' ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d)
	}
	if len(ids) != 2 {
		t.Fatalf("approval decisions=%v", ids)
	}
	return f, id, ids
}

func TestDecisionAnswerTwoApprovalsResumeAfterBothApproved(t *testing.T) {
	f, id, decisions := twoApprovalFixture(t)
	answer(t, f, decisions[0], `{"action":"approve","contentId":"content-1"}`, 200)
	requireState(t, f, id, "waiting_approval", "waiting_approval", "")
	if s := readState(t, f, id); s.ResumeStage.Valid {
		t.Fatalf("resume stage written early: %+v", s)
	}
	requireAnswered(t, f, decisions[0])
	answer(t, f, decisions[1], `{"action":"approve","contentId":"content-1"}`, 200)
	requireResume(t, f, id, "publish")
	requireAnswered(t, f, decisions[1])
	if n := count(t, f, `SELECT count(*) FROM approvals WHERE execution_id = ? AND status = 'approved'`, id); n != 2 {
		t.Fatalf("approved=%d", n)
	}
	if n := count(t, f, `SELECT count(*) FROM audit_logs WHERE action = 'approval.decide' AND actor = 'owner'`); n != 2 {
		t.Fatalf("approval audits=%d", n)
	}
}

func TestDecisionAnswerRejectWithdrawsOtherApproval(t *testing.T) {
	for _, approveFirst := range []bool{false, true} {
		name := "reject_first"
		if approveFirst {
			name = "after_one_approved"
		}
		t.Run(name, func(t *testing.T) {
			f, id, decisions := twoApprovalFixture(t)
			if approveFirst {
				answer(t, f, decisions[1], `{"action":"approve","contentId":"content-1"}`, 200)
			}
			answer(t, f, decisions[0], `{"action":"reject","contentId":"content-1"}`, 200)
			requireState(t, f, id, "failed", "failed", "approval_rejected")
			requireAnswered(t, f, decisions[0])
			if n := count(t, f, `SELECT count(*) FROM approvals WHERE decision_id = ? AND status = 'rejected'`, decisions[0]); n != 1 {
				t.Fatal("approval not rejected")
			}
			if n := count(t, f, `SELECT count(*) FROM approvals WHERE decision_id = ? AND status = 'invalidated'
				AND invalidated_reason = 'cancelled'`, decisions[1]); n != 1 {
				t.Fatal("other approval not invalidated")
			}
			if !approveFirst {
				if n := count(t, f, `SELECT count(*) FROM owner_decisions WHERE id = ? AND status = 'withdrawn'
					AND withdrawn_reason = 'cancelled'`, decisions[1]); n != 1 {
					t.Fatal("other decision not withdrawn")
				}
				answer(t, f, decisions[1], `{"action":"approve","contentId":"content-1"}`, 409)
			}
			if n := count(t, f, `SELECT count(*) FROM audit_logs WHERE action = 'approval.invalidate' AND actor = 'system'`); n != 1 {
				t.Fatalf("invalidate audits=%d", n)
			}
		})
	}
}
