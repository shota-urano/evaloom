package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Exercise real decision producers, including idempotent report delivery. Exact
// bodies ensure routing reasons, approval targets, findings and questions stay
// in the management UI. The last line is the spec 16 3.4 reconciliation key.
func TestLinearDecisionOpenComments(t *testing.T) {
	for _, host := range []string{"", "evaloom.example.ts.net"} {
		for _, source := range []string{"linear", "manual"} {
			for _, kind := range []string{"route", "approval", "blocked", "question", "publish_unknown"} {
				t.Run(host+"/"+source+"/"+kind, func(t *testing.T) {
					t.Cleanup(linearoutbox.SetRemoteHost(host))
					var f *authFixture
					var taskID, executionID, decisionID string
					if kind == "route" {
						var token, proposalID string
						f, token, proposalID, taskID = routeProposalFixture(t)
						execSQL(t, f, `UPDATE tasks SET source=? WHERE id=?`, source, taskID)
						for range 2 {
							status, body := proposalResult(t, f, token, proposalID, `{"label":"route:a","reason":"PRIVATE-ROUTE-REASON","errorClass":null}`)
							if status != 200 {
								t.Fatalf("proposal=%d %s", status, body)
							}
						}
						decisionID = routeDecisionOf(t, f, proposalID)
					} else {
						var token string
						f, token, executionID, taskID = reportFixture(t)
						if source == "linear" {
							linearTask(t, f, taskID)
						}
						var input map[string]any
						switch kind {
						case "question":
							attempt := startStage(t, f, token, executionID, "work")
							input = finished("work", attempt, nil)
							input["work"] = map[string]any{"sessionId": "s", "summaryArtifactId": nil,
								"question": map[string]any{"question": "PRIVATE-QUESTION", "options": []string{"PRIVATE-OPTION"}}}
						case "approval":
							approvalDeclarations(t, f, executionID)
							attempt := runTo(t, f, token, executionID, "check")
							input = finished("check", attempt, programResult("check", "needs_approval", map[string]any{
								"approvalRequests": []any{stageApprovalRequest("merge_pull_request")},
							}))
						case "blocked":
							attempt := runTo(t, f, token, executionID, "check")
							input = finished("check", attempt, programResult("check", "blocked", map[string]any{
								"summary": "PRIVATE-CHECK-SUMMARY", "message": "PRIVATE-BLOCK-REASON",
								"findings": []any{map[string]any{"rule": "spec_mismatch", "severity": "error", "message": "PRIVATE-FINDING"}},
							}))
						case "publish_unknown":
							attempt := runTo(t, f, token, executionID, "publish")
							input = finished("publish", attempt, programResult("publish", "unknown", map[string]any{
								"errorClass": "publish_unknown", "message": "PRIVATE-PUBLISH-REASON",
							}))
						}
						for range 2 {
							finishStage(t, f, token, executionID, input)
						}
						decisionKind := kind
						if kind == "publish_unknown" {
							decisionKind = "blocked"
						}
						decisionID = openDecisionID(t, f, executionID, decisionKind)
					}
					wantCount := 1
					if source == "manual" {
						wantCount = 0
					}
					if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND action='comment'`, taskID); n != wantCount {
						t.Fatalf("comment rows=%d; want %d", n, wantCount)
					}
					if source == "manual" || kind == "route" {
						if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND action='set_state'`, taskID); n != 0 {
							t.Fatalf("unexpected state rows=%d", n)
						}
					} else {
						requireLinearStateRow(t, f, taskID, executionID, "waiting_decision", decisionID, "In Review", "pending")
						if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND kind='waiting_decision' AND action='set_state'`, taskID); n != 1 {
							t.Fatalf("duplicate wait state rows=%d", n)
						}
					}
					if wantCount == 0 {
						return
					}
					var body, key, status string
					var execution sql.NullString
					if err := f.db.QueryRow(`SELECT body,dedupe_key,status,execution_id FROM linear_outbox
						WHERE task_id=? AND kind='decision_open' AND action='comment'`, taskID).Scan(&body, &key, &status, &execution); err != nil {
						t.Fatal(err)
					}
					if key != taskID+":decision_open:"+decisionID || status != "pending" || execution.String != executionID || execution.Valid != (executionID != "") {
						t.Fatalf("outbox key=%s status=%s execution=%+v", key, status, execution)
					}
					reference := decisionID
					if host != "" {
						reference = "https://" + host + "/decisions/" + decisionID
					}
					want := fmt.Sprintf("管理画面で判断待ち\n%s\n\n%s", reference, key)
					if body != want {
						t.Fatalf("body=%q; want %q", body, want)
					}
					if strings.Contains(body, "PRIVATE-") {
						t.Fatal("decision content leaked")
					}
					if host == "" {
						requireLinearRepliesLeaveDecisionOpen(t, f, taskID, decisionID)
					}
				})
			}
		}
	}
}

// A reply changes the Issue's update time. Use the real GraphQL decoder and
// intake/recheck path and verify that approval, continue and answer text cannot
// mutate a decision. The extra comments field also guards against accidentally
// adding comment-based inputs to intake in the future.
func requireLinearRepliesLeaveDecisionOpen(t *testing.T, f *authFixture, taskID, decisionID string) {
	t.Helper()
	before, err := store.GetOwnerDecision(t.Context(), f.db, decisionID)
	if err != nil {
		t.Fatal(err)
	}
	var taskStatus string
	if err := f.db.QueryRow(`SELECT status FROM tasks WHERE id=?`, taskID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	var reply string
	round := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Query string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if strings.Contains(request.Query, "comments") || !strings.Contains(request.Query, "EvaloomReadyIssues") {
			t.Errorf("intake requested comments: %s", request.Query)
		}
		comment, _ := json.Marshal(reply)
		fmt.Fprintf(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"ENG-1",
			"url":"https://linear.app/x/ENG-1","title":"Issue","description":"Proposal body",
			"updatedAt":"2026-11-20T00:0%d:00Z","team":{"id":"team-a"},"state":{"type":"started"},
			"labels":{"nodes":[{"name":"ai-ready"}],"pageInfo":{"hasNextPage":false}},
			"comments":{"nodes":[{"id":"reply","parentId":"decision-comment","body":%s}]}}],
			"pageInfo":{"hasNextPage":false}}}}`, round, comment)
	}))
	defer server.Close()
	keyPath := filepath.Join(t.TempDir(), "linear-key")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := linear.NewClient(server.Client(), server.URL, keyPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	starter := &linearintake.TaskStarter{}
	intake := &linearintake.Intake{DB: f.db, Fetcher: client, Starter: starter, Rechecker: starter,
		Canceller: linearintake.TaskCanceller{}}
	for i, text := range []string{"", `{"action":"approve","label":"route:a","contentId":"content-1"}`, `{"action":"continue"}`, `{"action":"answer","text":"keep"}`} {
		round, reply = i, text
		issues, err := client.ListReadyIssues(t.Context(), "team-a", "ai-ready")
		if err != nil {
			t.Fatal(err)
		}
		if err := intake.HandleTeamIssues(t.Context(), store.LinearPollTeam{TeamID: "team-a", BaselineDone: i > 0}, issues); err != nil {
			t.Fatal(err)
		}
		after, err := store.GetOwnerDecision(t.Context(), f.db, decisionID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("Linear reply changed decision: before=%+v after=%+v err=%v", before, after, err)
		}
		if n := count(t, f, `SELECT count(*) FROM tasks WHERE id=? AND status=?`, taskID, taskStatus); n != 1 {
			t.Fatal("Linear reply changed task state")
		}
		if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND action='comment'`, taskID); n != 1 {
			t.Fatalf("Linear reply created another comment: %d", n)
		}
	}
}

func TestLinearBlockedDecisionCommentsAdditionalCauses(t *testing.T) {
	for _, cause := range []string{"context_conflict", "workspace_missing_report", "workspace_missing_claim"} {
		t.Run(cause, func(t *testing.T) {
			t.Cleanup(linearoutbox.SetRemoteHost(""))
			var f *authFixture
			var executionID, taskID string
			if cause == "workspace_missing_claim" {
				var token string
				var job store.ExecutionJob
				f, token, job, _ = claimForResume(t, "work", func(f *authFixture, job store.ExecutionJob) {
					execSQL(t, f, `UPDATE task_workspaces SET status='cleaned',cleaned_at='2026-11-20T00:00:00Z' WHERE id=?`, job.Workspace.ID)
				})
				executionID, taskID = job.Execution.ID, job.Execution.TaskID
				linearTask(t, f, taskID)
				status, body := requestClaim(t, f, token)
				if status != 200 || !strings.Contains(string(body), `"job":null`) {
					t.Fatalf("claim=%d %s", status, body)
				}
			} else {
				var token string
				f, token, executionID, taskID = reportFixture(t)
				linearTask(t, f, taskID)
				if cause == "context_conflict" {
					input := contextInput(t, f, executionID)
					startStage(t, f, token, executionID, "work")
					files := input["files"].([]contextFile)
					input["files"] = append(files, contextFile{files[0].Kind, files[0].Path, strings.Repeat("d", 64)})
					status, body := requestContext(t, f, token, executionID, nil, input)
					assertAuthError(t, status, body, http.StatusConflict, Conflict)
				} else {
					execSQL(t, f, `UPDATE executions SET resume_stage='work' WHERE id=?`, executionID)
					attempt := startStage(t, f, token, executionID, "work")
					input := finished("work", attempt, nil)
					input["runnerError"], input["work"] = "workspace_missing", nil
					finishStage(t, f, token, executionID, input)
				}
			}
			requireState(t, f, executionID, "blocked", "blocked", "")
			decisionID := openDecisionID(t, f, executionID, "blocked")
			requireLinearStateRow(t, f, taskID, executionID, "waiting_decision", decisionID, "In Review", "pending")
			key := taskID + ":decision_open:" + decisionID
			wantBody := "管理画面で判断待ち\n" + decisionID + "\n\n" + key
			if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND kind='decision_open'
				AND action='comment' AND status='pending' AND dedupe_key=? AND body=? AND execution_id=?`, taskID, key, wantBody, executionID); n != 1 {
				t.Fatalf("decision comment rows=%d; want 1", n)
			}
		})
	}
}

func TestLinearDecisionCommentFailureRollsBackDecisionAndTransition(t *testing.T) {
	f, token, executionID, taskID := reportFixture(t)
	linearTask(t, f, taskID)
	attempt := startStage(t, f, token, executionID, "work")
	execSQL(t, f, `CREATE TRIGGER reject_decision_comment BEFORE INSERT ON linear_outbox
		WHEN NEW.kind='decision_open' BEGIN SELECT RAISE(ABORT,'outbox unavailable'); END`)
	input := finished("work", attempt, nil)
	input["work"] = map[string]any{"sessionId": "s", "summaryArtifactId": nil,
		"question": map[string]any{"question": "PRIVATE-QUESTION"}}
	status, body := postReport(t, f, token, executionID, input)
	if status != http.StatusInternalServerError {
		t.Fatalf("failed outbox=%d %s", status, body)
	}
	requireState(t, f, executionID, "working", "running", "")
	if n := count(t, f, `SELECT count(*) FROM owner_decisions WHERE execution_id=?`, executionID); n != 0 {
		t.Fatal("decision escaped rollback")
	}
	if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='decision_opened'`, executionID); n != 0 {
		t.Fatal("decision event escaped rollback")
	}
	execSQL(t, f, `DROP TRIGGER reject_decision_comment`)
	finishStage(t, f, token, executionID, input)
	if n := count(t, f, `SELECT count(*) FROM linear_outbox WHERE task_id=? AND kind='decision_open'`, taskID); n != 1 {
		t.Fatalf("retry comment rows=%d; want 1", n)
	}
}
