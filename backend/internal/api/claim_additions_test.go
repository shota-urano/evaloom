package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func requeueClaim(t *testing.T, f *authFixture, id string) {
	t.Helper()
	stageExec(t, f, `UPDATE executions SET status='pending' WHERE id=?`, id)
	stageExec(t, f, `UPDATE tasks SET status='queued' WHERE id=(SELECT task_id FROM executions WHERE id=?)`, id)
}

func TestClaimFreshNumberAcrossRetriesAndReclaims(t *testing.T) {
	f, token, _ := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "fresh-numbers")
	status, body := requestClaim(t, f, token)
	claim := requireClaim(t, status, body)
	if claim.Execution.FreshNumber != nil {
		t.Fatalf("initial freshNumber=%v", claim.Execution.FreshNumber)
	}
	for i, mode := range []string{"fresh", "continue", "fresh"} {
		stageExec(t, f, `UPDATE executions SET status='failed' WHERE id=?`, claim.Execution.ID)
		stageExec(t, f, `UPDATE tasks SET status='failed' WHERE id=?`, created.ID)
		status, body = requestRetry(t, f, created.ID, `{"mode":"`+mode+`"}`, nil)
		if status != http.StatusNoContent {
			t.Fatalf("retry=%d %s", status, body)
		}
		status, body = requestClaim(t, f, token)
		claim = requireClaim(t, status, body)
		if claim.Execution.Seq != i+2 || claim.Execution.Mode != mode {
			t.Fatalf("retry claim=%+v", claim.Execution)
		}
		if mode == "continue" {
			if claim.Execution.FreshNumber != nil {
				t.Fatalf("continue freshNumber=%v", claim.Execution.FreshNumber)
			}
		} else if claim.Execution.FreshNumber == nil || *claim.Execution.FreshNumber != i/2+1 {
			t.Fatalf("seq=%d freshNumber=%v", i+2, claim.Execution.FreshNumber)
		}
		requeueClaim(t, f, claim.Execution.ID)
		status, body = requestClaim(t, f, token)
		reclaimed := requireClaim(t, status, body)
		if reclaimed.Execution.ID != claim.Execution.ID || !reflect.DeepEqual(reclaimed.Execution.FreshNumber, claim.Execution.FreshNumber) {
			t.Fatalf("fresh number changed: before=%+v after=%+v", claim.Execution, reclaimed.Execution)
		}
	}
}

func TestClaimRegistryInputsAndNonResumeDefaults(t *testing.T) {
	f, token, _ := claimFixture(t)
	createClaimTask(t, f, "project-a", "registry-inputs")
	stageExec(t, f, `UPDATE harnesses SET tool_permissions='{"deny":["Bash(git push:*)"]}' WHERE id='harness-a'`)
	status, body := requestClaim(t, f, token)
	claim := requireClaim(t, status, body)
	harness, err := store.GetHarness(context.Background(), f.db, "harness-a")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Profile.HarnessRepoPath != harness.RepoPath || !reflect.DeepEqual(claim.Profile.ToolPermissions, harness.ToolPermissions) {
		t.Fatalf("profile=%+v harness=%+v", claim.Profile, harness)
	}
	var wire struct {
		Execution    map[string]json.RawMessage `json:"execution"`
		Project      map[string]json.RawMessage `json:"project"`
		PullRequests json.RawMessage            `json:"pullRequests"`
		Worktree     json.RawMessage            `json:"worktree"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"freshNumber", "resumeHeadSha", "harnessHeadSha", "verifiedSha", "verifyRun", "resumeBlock", "resumeHarnessSummary"} {
		if string(wire.Execution[field]) != "null" {
			t.Errorf("%s=%s; want explicit null", field, wire.Execution[field])
		}
	}
	if string(wire.Execution["resumeGateResults"]) != "[]" || string(wire.PullRequests) != "[]" ||
		string(wire.Project["setupAllowedHosts"]) != "[]" || string(wire.Worktree) != "null" {
		t.Fatalf("nonresume defaults=%s", body)
	}
}

func TestClaimResumeBlockAndGateResultsSnapshot(t *testing.T) {
	f, token, id, input := resumeFixture(t, "gate2_missing_tests")
	taskID := executionTaskID(t, f, id)
	// Gate number and lexical ID order deliberately differ from creation order.
	for _, row := range []struct {
		id   string
		gate int
		at   string
	}{
		{"z-first", 3, "2026-01-01T00:00:00Z"}, {"a-second", 2, "2026-01-01T00:00:00Z"},
	} {
		stageExec(t, f, `INSERT INTO gate_results(id,execution_id,gate,result,base_sha,target_sha,findings,created_at)
			VALUES(?,?,?,'blocked','base','target','[]',?)`, row.id, id, row.gate, row.at)
	}
	stageExec(t, f, `UPDATE execution_blocks SET gate_result_id='a-second',detail='{"missing":2}' WHERE id=?`, input.BlockID)
	stageExec(t, f, `UPDATE executions SET harness_head_sha='harness',verified_sha='verified' WHERE id=?`, id)
	status, body := requestResume(t, f, taskID, input)
	requireResumeSuccess(t, status, body)
	// A newer non-continue decision must not hide the approved block.
	stageExec(t, f, `INSERT INTO execution_blocks(id,execution_id,cause,head_sha,detail,decision,decided_at,created_at)
		VALUES('newer-abort',?,'git_state','wrong','{}','abort','2099-01-01T00:00:00Z','2099-01-01T00:00:00Z')`, id)
	stageExec(t, f, `INSERT INTO execution_blocks(id,execution_id,cause,head_sha,detail,decision,decided_at,created_at)
		VALUES('older-continue',?,'gate3_mismatch','older','{}','continue','2000-01-01T00:00:00Z','2000-01-01T00:00:00Z')`, id)
	status, body = requestClaim(t, f, token)
	claim := requireClaim(t, status, body)
	b := claim.Execution.ResumeBlock
	if claim.Execution.ID != id || claim.Execution.ResumePoint == nil || *claim.Execution.ResumePoint != "gate3" ||
		b == nil || b.ID != input.BlockID || b.Cause != "gate2_missing_tests" || b.HeadSHA != input.HeadSHA ||
		b.GateResultID == nil || *b.GateResultID != "a-second" || string(b.Detail) != `{"missing":2}` || b.DecidedAt == nil ||
		claim.Execution.ResumeHeadSHA == nil || *claim.Execution.ResumeHeadSHA != input.HeadSHA ||
		claim.Execution.HarnessHeadSHA == nil || *claim.Execution.HarnessHeadSHA != "harness" ||
		claim.Execution.VerifiedSHA == nil || *claim.Execution.VerifiedSHA != "verified" {
		t.Fatalf("resume input=%+v block=%+v", claim.Execution, b)
	}
	want := []store.ClaimGateResult{
		{ID: "z-first", Gate: 3, Result: "blocked", BaseSHA: "base", TargetSHA: "target", Findings: []store.Finding{}},
		{ID: "a-second", Gate: 2, Result: "blocked", BaseSHA: "base", TargetSHA: "target", Findings: []store.Finding{}},
	}
	if !reflect.DeepEqual(claim.Execution.ResumeGateResults, want) {
		t.Fatalf("gates=%+v want=%+v", claim.Execution.ResumeGateResults, want)
	}
	// Saved evidence without a resume point must not leak into a normal claim.
	requeueClaim(t, f, id)
	stageExec(t, f, `UPDATE executions SET resume_point=NULL WHERE id=?`, id)
	status, body = requestClaim(t, f, token)
	claim = requireClaim(t, status, body)
	if claim.Execution.ResumeBlock != nil || len(claim.Execution.ResumeGateResults) != 0 || claim.Execution.ResumeHeadSHA != nil ||
		claim.Execution.HarnessHeadSHA != nil || claim.Execution.VerifiedSHA != nil || claim.Execution.VerifyRun != nil {
		t.Fatalf("nonresume evidence=%+v", claim.Execution)
	}
}

func TestClaimPullRequestsDeduplicateAndOrderHistory(t *testing.T) {
	for _, mode := range []string{"continue", "fresh"} {
		t.Run(mode, func(t *testing.T) {
			f, token, _ := claimFixture(t)
			created := createClaimTask(t, f, "project-a", "pr-history")
			stageExec(t, f, `UPDATE executions SET status='failed',pr_number=12,branch_name='old-12' WHERE task_id=?`, created.ID)
			for _, row := range []struct {
				seq, number int
				branch      string
			}{{2, 34, "branch-34"}, {3, 12, "new-12"}} {
				stageExec(t, f, `INSERT INTO executions(id,task_id,seq,mode,status,pr_number,branch_name,created_at)
					VALUES(?,?,?,'continue','failed',?,?,'2026-01-01T00:00:00Z')`, ulid.Make().String(), created.ID, row.seq, row.number, row.branch)
			}
			current := ulid.Make().String()
			stageExec(t, f, `INSERT INTO executions(id,task_id,seq,mode,status,pr_number,created_at)
				VALUES(?,?,4,?,'pending',99,'2026-01-01T00:00:00Z')`, current, created.ID, mode)
			status, body := requestClaim(t, f, token)
			claim := requireClaim(t, status, body)
			branch12, branch34 := "new-12", "branch-34"
			want := []store.ClaimPullRequest{{Number: 12, BranchName: &branch12, Seq: 3}, {Number: 34, BranchName: &branch34, Seq: 2}}
			if claim.Execution.ID != current || !reflect.DeepEqual(claim.PullRequests, want) {
				t.Fatalf("claim=%s PRs=%+v want=%+v", claim.Execution.ID, claim.PullRequests, want)
			}
		})
	}
}

func TestClaimExpectedHeadSHAFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name            string
		pushed, harness *string
		want            string
	}{
		{"pushed", new("pushed"), new("harness"), "pushed"},
		{"harness", nil, new("harness"), "harness"},
		{"base", nil, nil, strings.Repeat("a", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, previous := retryFixture(t, "failed")
			stageExec(t, f, `UPDATE executions SET pushed_sha=?,harness_head_sha=? WHERE id=?`, tc.pushed, tc.harness, previous)
			status, body := requestRetry(t, f, executionTaskID(t, f, previous), `{"mode":"continue"}`, nil)
			if status != http.StatusNoContent {
				t.Fatalf("retry=%d %s", status, body)
			}
			status, body = requestClaim(t, f, token)
			w := requireClaim(t, status, body).Worktree
			if w == nil || w.ExpectedHeadSHA == nil || *w.ExpectedHeadSHA != tc.want || w.Status != "retained" {
				t.Fatalf("worktree=%+v want SHA=%s", w, tc.want)
			}
		})
	}
}

func TestClaimPreparationCompleteTracksRestoreGeneration(t *testing.T) {
	f, token, id, input := createdReport(t)
	stageExec(t, f, `UPDATE projects SET setup_command='' WHERE id='project-a'`)
	stageOK(t, f, token, id, input)
	wid := input["worktree"].(map[string]any)["id"].(string)
	stageOK(t, f, token, id, resultBody("preparing", "failed"))
	status, body := requestRetry(t, f, executionTaskID(t, f, id), `{"mode":"continue"}`, nil)
	if status != http.StatusNoContent {
		t.Fatalf("retry=%d %s", status, body)
	}
	status, body = requestClaim(t, f, token)
	claim := requireClaim(t, status, body)
	if claim.Worktree == nil || !claim.Worktree.PreparationComplete {
		t.Fatalf("initial preparation=%+v", claim.Worktree)
	}
	current := claim.Execution.ID
	stageOK(t, f, token, current, resultBody("claimed", "preparing"))
	stageOK(t, f, token, current, map[string]any{"fromStatus": "preparing", "status": "preparing", "worktree": map[string]any{"action": "restore_started", "id": wid}})
	// Even a stale succeeded status cannot make the old ready event valid.
	stageExec(t, f, `UPDATE worktrees SET setup_status='succeeded' WHERE id=?`, wid)
	requeueClaim(t, f, current)
	status, body = requestClaim(t, f, token)
	claim = requireClaim(t, status, body)
	if claim.Worktree.PreparationComplete {
		t.Fatal("unfinished restore reused old ready event")
	}
	stageExec(t, f, `UPDATE worktrees SET setup_status='pending' WHERE id=?`, wid)
	stageOK(t, f, token, current, resultBody("claimed", "preparing"))
	restored := map[string]any{"fromStatus": "preparing", "status": "preparing", "worktree": map[string]any{
		"action": "restored", "id": wid, "path": "/worktrees/new", "branchName": "evaloom/task-new", "baseSha": strings.Repeat("a", 40), "copiedFiles": []any{},
	}}
	stageOK(t, f, token, current, restored)
	requeueClaim(t, f, current)
	status, body = requestClaim(t, f, token)
	claim = requireClaim(t, status, body)
	if !claim.Worktree.PreparationComplete {
		t.Fatal("restored worktree with setup skip was not ready")
	}
	stageExec(t, f, `UPDATE worktrees SET setup_status='failed' WHERE id=?`, wid)
	requeueClaim(t, f, current)
	status, body = requestClaim(t, f, token)
	if requireClaim(t, status, body).Worktree.PreparationComplete {
		t.Fatal("failed setup marked ready")
	}
}

func TestClaimResumeHarnessSummaryArtifactFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          *string
	}{
		{"summary", `{"summary":"作業の概要","commits":[]}`, new("作業の概要")},
		{"empty", `{"summary":""}`, new("")},
		{"missing", "", nil}, {"unreadable", `{"summary":"lost"}`, nil},
		{"invalid_json", `{`, nil}, {"invalid_summary", `{"summary":1}`, nil},
		{"no_summary", `{}`, nil}, {"invalid_utf8", "{\"summary\":\"\xff\"}", nil},
		{"wrong_kind", `{"summary":"wrong kind"}`, nil},
		{"store_unconfigured", `{"summary":"not available"}`, nil},
		{"trailing_json", `{"summary":"first"} {}`, nil},
		{"multiple", `{"summary":"ambiguous"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, id, input := resumeFixture(t, "gate2_unverifiable")
			dir := t.TempDir()
			files, err := store.NewArtifactStore(f.db, dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { files.Close() })
			// Use the full authenticated claim handler with the artifact store.
			if tc.name != "store_unconfigured" {
				f.server.Config.Handler = NewHandlerWithArtifacts(f.db, nil, f.auth, nil, ContextConfig{}, ArtifactConfig{Store: files, MaxBytes: 1024}, f.adapters)
			}
			if tc.name != "missing" {
				kind := "harness_output"
				if tc.name == "wrong_kind" {
					kind = "harness_log"
				}
				artifact, err := files.Save(context.Background(), ulid.Make().String(), id, kind, false, strings.NewReader(tc.content))
				if err != nil {
					t.Fatal(err)
				}
				if tc.name == "unreadable" {
					// Metadata exists, but its file no longer resolves inside the store.
					stageExec(t, f, `UPDATE artifacts SET storage_path=? WHERE id=?`, filepath.Join("artifacts", "missing"), artifact.ID)
				}
				if tc.name == "multiple" {
					if _, err := files.Save(context.Background(), ulid.Make().String(), id, "harness_output", false, strings.NewReader(tc.content)); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Another Execution's artifact must never substitute for this one's.
			other := createClaimTask(t, f, "project-b", "other-summary")
			var otherID string
			if err := f.db.QueryRow(`SELECT id FROM executions WHERE task_id=?`, other.ID).Scan(&otherID); err != nil {
				t.Fatal(err)
			}
			stageExec(t, f, `UPDATE executions SET status='failed' WHERE id=?`, otherID)
			if _, err := files.Save(context.Background(), ulid.Make().String(), otherID, "harness_output", false, strings.NewReader(`{"summary":"other"}`)); err != nil {
				t.Fatal(err)
			}
			status, body := requestResume(t, f, executionTaskID(t, f, id), input)
			requireResumeSuccess(t, status, body)
			status, body = requestClaim(t, f, token)
			claim := requireClaim(t, status, body)
			if claim.Execution.ID != id || !reflect.DeepEqual(claim.Execution.ResumeHarnessSummary, tc.want) {
				t.Fatalf("summary=%v want=%v body=%s", claim.Execution.ResumeHarnessSummary, tc.want, body)
			}
			if tc.want != nil {
				requeueClaim(t, f, id)
				stageExec(t, f, `UPDATE executions SET resume_point=NULL WHERE id=?`, id)
				status, body = requestClaim(t, f, token)
				if requireClaim(t, status, body).Execution.ResumeHarnessSummary != nil {
					t.Fatal("nonresume claim loaded harness summary")
				}
			}
		})
	}
}

func TestClaimPreparationCompleteRequiresMatchingReadyAndSetup(t *testing.T) {
	for _, tc := range []struct {
		name, mutate string
		want         bool
	}{
		{"succeeded", `UPDATE worktrees SET setup_status='succeeded'`, true},
		{"skipped", `UPDATE projects SET setup_command='now-required'`, true},
		{"failed", `UPDATE worktrees SET setup_status='failed'`, false},
		{"pending_without_skip", `UPDATE execution_events SET payload=json_set(payload,'$.sourceId','other') WHERE type='log'`, false},
		{"wrong_path", `UPDATE execution_events SET payload=json_set(payload,'$.path','/wrong') WHERE type='worktree_ready'`, false},
		{"wrong_branch", `UPDATE execution_events SET payload=json_set(payload,'$.branch','wrong') WHERE type='worktree_ready'`, false},
		{"wrong_base", `UPDATE execution_events SET payload=json_set(payload,'$.baseSha','wrong') WHERE type='worktree_ready'`, false},
		{"cleaned", `UPDATE worktrees SET status='cleaned'`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, id, input := createdReport(t)
			stageExec(t, f, `UPDATE projects SET setup_command='' WHERE id='project-a'`)
			stageOK(t, f, token, id, input)
			stageExec(t, f, tc.mutate)
			requeueClaim(t, f, id)
			status, body := requestClaim(t, f, token)
			w := requireClaim(t, status, body).Worktree
			if w == nil || w.PreparationComplete != tc.want {
				t.Fatalf("worktree=%+v want ready=%v", w, tc.want)
			}
			if tc.name == "cleaned" && w.Status != "cleaned" {
				t.Fatalf("status=%s", w.Status)
			}
		})
	}
}
