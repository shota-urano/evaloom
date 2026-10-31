package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

func claimFixture(t *testing.T) (*authFixture, string, []model.HarnessProfile) {
	t.Helper()
	f, _, profiles := assignmentFixture(t)
	token, err := runnerauth.Register(context.Background(), f.db, "claim-runner", "Claim Runner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE projects SET runner_id = 'claim-runner'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"project-a", "project-b"} {
		status, body := postAssignment(t, f, id, "developer", "harness-a", profiles[0].Version)
		requireAssignment(t, status, body, 201)
	}
	return f, token, profiles
}

func createClaimTask(t *testing.T, f *authFixture, project, key string) model.Task {
	t.Helper()
	input := manualTaskInput()
	input["projectId"] = project
	status, body := taskRequest(t, f, key, input, nil)
	return requireTask(t, status, body, 201)
}

func requestClaim(t *testing.T, f *authFixture, token string) (int, []byte) {
	t.Helper()
	return f.request(t, "POST", "/api/runners/claim-runner/claim", false, "", func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer "+token)
	})
}

func requireClaim(t *testing.T, status int, body []byte) store.Claim {
	t.Helper()
	var claim store.Claim
	if status != 200 || json.Unmarshal(body, &claim) != nil || claim.Execution.ID == "" {
		t.Fatalf("claim status=%d body=%s", status, body)
	}
	return claim
}

type executionPin struct {
	Runner, Role, Harness, Profile, Version, SHA, ClaimedAt string
}

func readExecutionPin(t *testing.T, f *authFixture, id string) executionPin {
	t.Helper()
	var pin executionPin
	err := f.db.QueryRow(`SELECT runner_id, role, harness_id, profile_id, profile_version,
		profile_commit_sha, claimed_at FROM executions WHERE id = ?`, id).
		Scan(&pin.Runner, &pin.Role, &pin.Harness, &pin.Profile, &pin.Version, &pin.SHA, &pin.ClaimedAt)
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

func TestClaimConcurrentLimitsAndCreationOrder(t *testing.T) {
	f, token, _ := claimFixture(t)
	for _, id := range []string{"project-c", "project-d"} {
		input := projectInput(t, id)
		input["runnerId"] = "claim-runner"
		status, body := projectRequest(t, f, "POST", "/api/projects", input)
		requireProject(t, status, body, 201)
		status, body = postAssignment(t, f, id, "developer", "harness-a", "1.2.0")
		requireAssignment(t, status, body, 201)
	}
	var tasks []model.Task
	for i, project := range []string{"project-a", "project-a", "project-b", "project-c", "project-d"} {
		created := createClaimTask(t, f, project, fmt.Sprintf("parallel-%d", i))
		tasks = append(tasks, created)
		if _, err := f.db.Exec(`UPDATE executions SET created_at = ? WHERE task_id = ?`,
			fmt.Sprintf("2026-10-2%dT00:00:00Z", i), created.ID); err != nil {
			t.Fatal(err)
		}
	}
	var pending int
	if err := f.db.QueryRow(`SELECT count(*) FROM executions WHERE status = 'pending'`).Scan(&pending); err != nil || pending != 5 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	type result struct {
		status int
		body   []byte
	}
	results := make(chan result, 5)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, body := requestClaim(t, f, token)
			results <- result{status, body}
			// Observe capacity during concurrent requests, not only at the end.
			var active, maxProject int
			err := f.db.QueryRow(`SELECT coalesce(sum(n), 0), coalesce(max(n), 0) FROM (
				SELECT count(*) n FROM executions e JOIN tasks t ON t.id = e.task_id
				WHERE e.status IN ('claimed','preparing','running','verifying','publishing')
				GROUP BY t.project_id)`).Scan(&active, &maxProject)
			if err != nil || active > 2 || maxProject > 1 {
				t.Errorf("capacity during claim: active=%d maxProject=%d err=%v", active, maxProject, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	claimedTasks := map[string]bool{}
	empty := 0
	for result := range results {
		if result.status == 204 && len(result.body) == 0 {
			empty++
			continue
		}
		claim := requireClaim(t, result.status, result.body)
		if claimedTasks[claim.Execution.TaskID] {
			t.Fatal("Execution was claimed twice")
		}
		claimedTasks[claim.Execution.TaskID] = true
	}
	if len(claimedTasks) != 2 || empty != 3 || !claimedTasks[tasks[0].ID] || !claimedTasks[tasks[2].ID] {
		t.Fatalf("claimed=%v empty=%d; want oldest eligible tasks %s and %s", claimedTasks, empty, tasks[0].ID, tasks[2].ID)
	}
	var claimed, queued, events, audits, mismatches int
	for _, check := range []struct {
		query string
		dest  *int
		want  int
	}{
		{`SELECT count(*) FROM executions WHERE status = 'claimed'`, &claimed, 2},
		{`SELECT count(*) FROM tasks WHERE status = 'queued'`, &queued, 3},
		{`SELECT count(*) FROM execution_events WHERE json_extract(payload, '$.reason') = 'runner_claimed'`, &events, 2},
		{`SELECT count(*) FROM audit_logs WHERE action = 'execution.claim'`, &audits, 2},
		{`SELECT count(*) FROM tasks t JOIN executions e ON e.task_id = t.id
			WHERE (e.status = 'claimed' AND t.status != 'running') OR (e.status = 'pending' AND t.status != 'queued')`, &mismatches, 0},
	} {
		if err := f.db.QueryRow(check.query).Scan(check.dest); err != nil || *check.dest != check.want {
			t.Fatalf("query=%s got=%d want=%d err=%v", check.query, *check.dest, check.want, err)
		}
	}
}

func TestClaimPinsCurrentAssignmentAndLaterChangesPreserveExecution(t *testing.T) {
	f, token, profiles := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "pin")
	// A queued task picks the assignment at claim, not at task creation.
	status, body := postAssignment(t, f, "project-a", "developer", "harness-a", profiles[1].Version)
	requireAssignment(t, status, body, 200)
	status, body = requestClaim(t, f, token)
	claim := requireClaim(t, status, body)
	pin := readExecutionPin(t, f, claim.Execution.ID)
	want := executionPin{"claim-runner", "developer", "harness-a", profiles[1].ID, profiles[1].Version, profiles[1].CommitSHA, pin.ClaimedAt}
	if pin != want || pin.ClaimedAt == "" {
		t.Fatalf("saved pin=%+v want=%+v", pin, want)
	}
	if _, err := ParseTimestamp(pin.ClaimedAt); err != nil {
		t.Fatal(err)
	}
	project, err := store.GetProject(context.Background(), f.db, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Execution.TaskID != created.ID || claim.Execution.Seq != 1 || claim.Execution.Mode != "continue" || claim.Execution.ResumePoint != nil || claim.Previous != nil ||
		claim.Task != (store.ClaimTask{Title: created.Title, Body: created.Body, AcceptanceCriteria: created.AcceptanceCriteria, ShortName: created.ShortName}) || !reflect.DeepEqual(claim.Project, project) ||
		claim.Profile.HarnessID != pin.Harness || claim.Profile.Adapter != "codex" || claim.Profile.Version != pin.Version || claim.Profile.CommitSHA != pin.SHA ||
		!reflect.DeepEqual(claim.Profile.Definition, profiles[1].Definition) {
		t.Fatalf("response does not match stored input: %+v", claim)
	}
	if strings.Contains(string(body), token) || strings.Contains(string(body), "tokenHash") {
		t.Fatal("credentials leaked")
	}
	var event, audit string
	err = f.db.QueryRow(`SELECT payload FROM execution_events WHERE execution_id = ? AND json_extract(payload, '$.reason') = 'runner_claimed'`, claim.Execution.ID).Scan(&event)
	if err != nil || event != `{"from":"pending","to":"claimed","reason":"runner_claimed"}` {
		t.Fatalf("event=%s err=%v", event, err)
	}
	err = f.db.QueryRow(`SELECT detail FROM audit_logs WHERE actor = 'runner:claim-runner' AND action = 'execution.claim' AND target_type = 'execution' AND target_id = ?`, claim.Execution.ID).Scan(&audit)
	var detail struct {
		ProfileID        string
		ProfileCommitSHA string
	}
	if err != nil || json.Unmarshal([]byte(audit), &detail) != nil || detail.ProfileID != pin.Profile || detail.ProfileCommitSHA != pin.SHA {
		t.Fatalf("audit=%s err=%v", audit, err)
	}
	status, body = postAssignment(t, f, "project-a", "developer", "harness-a", profiles[0].Version)
	requireAssignment(t, status, body, 200)
	if storedAssignment(t, f, "project-a", "developer").ProfileID != profiles[0].ID || readExecutionPin(t, f, claim.Execution.ID) != pin {
		t.Fatal("assignment replacement rewrote Execution's saved version")
	}
	status, body = requestClaim(t, f, token)
	if status != 204 || len(body) != 0 || readExecutionPin(t, f, claim.Execution.ID) != pin {
		t.Fatalf("repeated claim status=%d body=%s", status, body)
	}
}

func TestClaimResumePreservesPinnedProfile(t *testing.T) {
	for _, tc := range []struct{ name, resumePoint, role string }{
		{"gate3_role_unchanged", "gate3", "unchanged"},
		{"gate3_role_changed", "gate3", "assigned"},
		{"publish_role_missing", "publish", "missing"},
	} {
		resumePoint := tc.resumePoint
		t.Run(tc.name, func(t *testing.T) {
			f, token, profiles := claimFixture(t)
			created := createClaimTask(t, f, "project-a", "resume-pin")
			status, body := requestClaim(t, f, token)
			first := requireClaim(t, status, body)
			pin := readExecutionPin(t, f, first.Execution.ID)
			if pin.Profile != profiles[0].ID || pin.Version != profiles[0].Version || pin.SHA != profiles[0].CommitSHA {
				t.Fatalf("initial pin=%+v", pin)
			}
			ctx := context.Background()
			// Use the lifecycle functions to reach a real Blocked Execution, then
			// queue that same Execution as the resume handler will do (spec 04, 3.8).
			from := model.ExecutionClaimed
			for _, to := range []model.ExecutionStatus{model.ExecutionPreparing, model.ExecutionRunning, model.ExecutionVerifying, model.ExecutionBlocked} {
				fromTask, err := task.TaskStatusForExecution(from)
				if err != nil {
					t.Fatal(err)
				}
				if err := task.TransitionExecution(ctx, f.db, task.Transition{
					TaskID: created.ID, ExecutionID: first.Execution.ID,
					FromTask: fromTask, FromExecution: from, ToExecution: to,
				}); err != nil {
					t.Fatal(err)
				}
				from = to
			}
			// Retain an existing Harness Context to check its correspondence with
			// the Profile returned by the second HTTP claim, as well as its DB pin.
			revision := store.ContextRevision{
				ID: ulid.Make().String(), ExecutionID: first.Execution.ID,
				Layer: "harness", Kind: "harness_prompt", SourcePath: "prompt.md",
				SourceCommitSHA: pin.SHA, Content: "Pinned instructions", ContentHash: contextHash("Pinned instructions"),
				CreatedAt: pin.ClaimedAt,
			}
			tx, err := f.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := store.InsertContextRevision(ctx, tx, revision); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			status, body = postAssignment(t, f, "project-a", "developer", "harness-a", profiles[1].Version)
			requireAssignment(t, status, body, 200)
			if storedAssignment(t, f, "project-a", "developer").ProfileID != profiles[1].ID {
				t.Fatal("assignment did not change to version B")
			}
			if tc.role != "unchanged" {
				// A changed or missing current Default Role must not replace the
				// Execution's original role or prevent its continuation.
				if tc.role == "assigned" {
					status, body = postAssignment(t, f, "project-a", "reviewer", "harness-a", profiles[1].Version)
					requireAssignment(t, status, body, 201)
				}
				status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"defaultRole": "reviewer"})
				requireProject(t, status, body, 200)
			}
			tx, err = f.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, `UPDATE executions SET resume_point = ? WHERE id = ?`, resumePoint, first.Execution.ID); err != nil {
				t.Fatal(err)
			}
			if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
				TaskID: created.ID, ExecutionID: first.Execution.ID,
				FromTask: model.TaskBlocked, FromExecution: model.ExecutionBlocked, ToExecution: model.ExecutionPending,
			}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			status, body = requestClaim(t, f, token)
			resumed := requireClaim(t, status, body)
			if resumed.Execution.ID != first.Execution.ID || resumed.Execution.TaskID != created.ID || resumed.Execution.Seq != first.Execution.Seq ||
				resumed.Execution.ResumePoint == nil || *resumed.Execution.ResumePoint != resumePoint || !reflect.DeepEqual(resumed.Profile, first.Profile) {
				t.Fatalf("resumed claim=%+v; original profile=%+v", resumed, first.Profile)
			}
			after := readExecutionPin(t, f, first.Execution.ID)
			want := pin
			want.ClaimedAt = after.ClaimedAt // The new claim may update its acquisition time.
			if after != want || after.ClaimedAt == "" {
				t.Fatalf("resume changed pin: got=%+v want=%+v", after, want)
			}
			var executionStatus, taskStatus, savedResume string
			var count int
			err = f.db.QueryRow(`SELECT e.status, t.status, e.resume_point,
				(SELECT count(*) FROM executions WHERE task_id = t.id)
				FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, first.Execution.ID).
				Scan(&executionStatus, &taskStatus, &savedResume, &count)
			if err != nil || executionStatus != "claimed" || taskStatus != "running" || savedResume != resumePoint || count != 1 {
				t.Fatalf("execution=%s task=%s resume=%s count=%d err=%v", executionStatus, taskStatus, savedResume, count, err)
			}
			revisions, err := store.ListContextRevisions(ctx, f.db, first.Execution.ID)
			if err != nil || !reflect.DeepEqual(revisions, []store.ContextRevision{revision}) || revisions[0].SourceCommitSHA != resumed.Profile.CommitSHA {
				t.Fatalf("resumed Profile no longer matches retained Context: revisions=%+v err=%v", revisions, err)
			}
			var auditProfile, auditSHA string
			err = f.db.QueryRow(`SELECT json_extract(detail, '$.profileId'), json_extract(detail, '$.profileCommitSha')
				FROM audit_logs WHERE action = 'execution.claim' AND target_id = ? ORDER BY id DESC LIMIT 1`, first.Execution.ID).
				Scan(&auditProfile, &auditSHA)
			if err != nil || auditProfile != pin.Profile || auditSHA != pin.SHA {
				t.Fatalf("resume audit profile=%s sha=%s err=%v", auditProfile, auditSHA, err)
			}
		})
	}
}

func TestClaimCountsAllActiveStatesAndReleasesBlockedAndTerminalSlots(t *testing.T) {
	f, token, _ := claimFixture(t)
	a := createClaimTask(t, f, "project-a", "active-a")
	aPending := createClaimTask(t, f, "project-a", "pending-a")
	b := createClaimTask(t, f, "project-b", "pending-b")
	// Force a deterministic first pick independent of the generated ULIDs.
	for i, id := range []string{a.ID, aPending.ID, b.ID} {
		if _, err := f.db.Exec(`UPDATE executions SET created_at = ? WHERE task_id = ?`, fmt.Sprintf("202%d-01-01T00:00:00Z", i), id); err != nil {
			t.Fatal(err)
		}
	}
	status, body := requestClaim(t, f, token)
	first := requireClaim(t, status, body)
	for _, state := range []string{"claimed", "preparing", "running", "verifying", "publishing"} {
		t.Run(state, func(t *testing.T) {
			if _, err := f.db.Exec(`UPDATE executions SET status = ? WHERE task_id = ?`, state, a.ID); err != nil {
				t.Fatal(err)
			}
			status, body := requestClaim(t, f, token)
			claim := requireClaim(t, status, body)
			if claim.Execution.TaskID != b.ID {
				t.Fatalf("active Project was not skipped: %+v", claim)
			}
			// Global capacity includes active work assigned to another Runner.
			if _, err := f.db.Exec(`UPDATE executions SET runner_id = 'mac-mini', status = ? WHERE task_id = ?`, state, b.ID); err != nil {
				t.Fatal(err)
			}
			status, body = requestClaim(t, f, token)
			if status != 204 || len(body) != 0 {
				t.Fatalf("global limit status=%d body=%s", status, body)
			}
			if _, err := f.db.Exec(`UPDATE executions SET status = 'pending' WHERE task_id = ?`, b.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE tasks SET status = 'queued' WHERE id = ?`, b.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, state := range []string{"blocked", "succeeded", "failed", "interrupted", "cancelled", "timed_out"} {
		t.Run(state, func(t *testing.T) {
			if _, err := f.db.Exec(`UPDATE executions SET status = ? WHERE id = ?`, state, first.Execution.ID); err != nil {
				t.Fatal(err)
			}
			status, body := requestClaim(t, f, token)
			claim := requireClaim(t, status, body)
			if claim.Project.ID != "project-a" || claim.Execution.ID == first.Execution.ID {
				t.Fatalf("released Project was not claimable: %+v", claim)
			}
			if _, err := f.db.Exec(`UPDATE executions SET status = 'pending' WHERE id = ?`, claim.Execution.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE tasks SET status = 'queued' WHERE id = ?`, claim.Execution.TaskID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClaimFiltersProjectRunner(t *testing.T) {
	f, token, _ := claimFixture(t)
	a := createClaimTask(t, f, "project-a", "foreign")
	b := createClaimTask(t, f, "project-b", "own")
	if _, err := f.db.Exec(`UPDATE projects SET runner_id = 'mac-mini' WHERE id = 'project-a'`); err != nil {
		t.Fatal(err)
	}
	status, body := requestClaim(t, f, token)
	if claim := requireClaim(t, status, body); claim.Execution.TaskID != b.ID {
		t.Fatalf("claim=%+v", claim)
	}
	status, body = requestClaim(t, f, token)
	if status != 204 || len(body) != 0 {
		t.Fatalf("foreign task claim status=%d body=%s", status, body)
	}
	var untouched int
	if err := f.db.QueryRow(`SELECT count(*) FROM executions WHERE task_id = ? AND status = 'pending' AND runner_id IS NULL AND profile_id IS NULL`, a.ID).Scan(&untouched); err != nil || untouched != 1 {
		t.Fatalf("untouched=%d err=%v", untouched, err)
	}
}

func assertUnclaimed(t *testing.T, f *authFixture, taskID string) {
	t.Helper()
	var untouched, events, audits int
	err := f.db.QueryRow(`SELECT count(*) FROM executions e JOIN tasks t ON t.id = e.task_id
		WHERE t.id = ? AND t.status = 'queued' AND e.status = 'pending' AND e.runner_id IS NULL
		AND e.role IS NULL AND e.harness_id IS NULL AND e.profile_id IS NULL
		AND e.profile_version IS NULL AND e.profile_commit_sha IS NULL AND e.claimed_at IS NULL`, taskID).Scan(&untouched)
	if err != nil || untouched != 1 {
		t.Fatalf("unclaimed=%d err=%v", untouched, err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE json_extract(payload, '$.reason') = 'runner_claimed'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action = 'execution.claim'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if events != 0 || audits != 0 {
		t.Fatalf("failed claim events=%d audits=%d", events, audits)
	}
}

func TestClaimRollsBackEveryWriteOnFailure(t *testing.T) {
	f, token, _ := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "rollback-claim")
	for _, trigger := range []string{
		`BEFORE UPDATE OF status ON tasks WHEN NEW.status = 'running'`,
		`BEFORE UPDATE OF profile_id ON executions`,
		`BEFORE INSERT ON execution_events WHEN json_extract(NEW.payload, '$.reason') = 'runner_claimed'`,
		`BEFORE INSERT ON audit_logs WHEN NEW.action = 'execution.claim'`,
	} {
		if _, err := f.db.Exec(`CREATE TRIGGER reject_claim ` + trigger + ` BEGIN SELECT RAISE(ABORT, 'claim storage unavailable'); END`); err != nil {
			t.Fatal(err)
		}
		status, body := requestClaim(t, f, token)
		requireProjectError(t, status, body, 500, Internal, "", "")
		if strings.Contains(string(body), "storage unavailable") {
			t.Fatal("internal error disclosed")
		}
		assertUnclaimed(t, f, created.ID)
		if _, err := f.db.Exec(`DROP TRIGGER reject_claim`); err != nil {
			t.Fatal(err)
		}
	}
	status, body := requestClaim(t, f, token)
	requireClaim(t, status, body)
}

func TestClaimMissingAssignmentAndStateConflictLeaveNoPins(t *testing.T) {
	f, token, _ := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "preconditions")
	status, body := projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"defaultRole": "unassigned"})
	requireProject(t, status, body, 200)
	status, body = requestClaim(t, f, token)
	requireProjectError(t, status, body, 422, PreconditionFailed, "projectId", "default_harness_missing")
	assertUnclaimed(t, f, created.ID)
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"defaultRole": "developer"})
	requireProject(t, status, body, 200)
	if _, err := f.db.Exec(`UPDATE tasks SET status = 'cancelled' WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	status, body = requestClaim(t, f, token)
	requireProjectError(t, status, body, 409, Conflict, "", "")
	if _, err := f.db.Exec(`UPDATE tasks SET status = 'queued' WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	assertUnclaimed(t, f, created.ID)
}

func TestClaimPreviousExecutionAndResumePoint(t *testing.T) {
	for _, mode := range []string{"continue", "fresh"} {
		t.Run(mode, func(t *testing.T) {
			f, token, profiles := claimFixture(t)
			created := createClaimTask(t, f, "project-a", "previous")
			status, body := requestClaim(t, f, token)
			previous := requireClaim(t, status, body)
			worktreeID, nextID := ulid.Make().String(), ulid.Make().String()
			path, branch, sha := "/worktrees/previous", "evaloom/previous", strings.Repeat("a", 40)
			_, err := f.db.Exec(`INSERT INTO worktrees
				(id, project_id, task_id, runner_id, path, branch_name, base_sha, status, setup_status, copied_files, created_at)
				VALUES (?, 'project-a', ?, 'claim-runner', ?, ?, ?, 'retained', 'succeeded', '[]', '2026-10-27T00:00:00Z')`, worktreeID, created.ID, path, branch, sha)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.db.Exec(`UPDATE executions SET status = 'failed', worktree_id = ?, branch_name = ?,
				pr_number = 12 WHERE id = ?`, worktreeID, branch, previous.Execution.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.db.Exec(`INSERT INTO executions (id, task_id, seq, mode, status, resume_point, created_at)
				VALUES (?, ?, 2, ?, 'pending', 'gate3', '2026-10-27T00:00:00Z')`, nextID, created.ID, mode)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE tasks SET status = 'queued' WHERE id = ?`, created.ID); err != nil {
				t.Fatal(err)
			}
			// A new Execution still selects the current assignment, even when
			// its predecessor has a different pin and resume_point is populated.
			status, body = postAssignment(t, f, "project-a", "developer", "harness-a", profiles[1].Version)
			requireAssignment(t, status, body, 200)
			status, body = requestClaim(t, f, token)
			claim := requireClaim(t, status, body)
			if claim.Execution.ID != nextID || claim.Execution.Seq != 2 || claim.Execution.Mode != mode || claim.Execution.ResumePoint == nil || *claim.Execution.ResumePoint != "gate3" {
				t.Fatalf("retry claim=%+v", claim)
			}
			pin := readExecutionPin(t, f, nextID)
			if pin.Profile != profiles[1].ID || pin.Version != profiles[1].Version || pin.SHA != profiles[1].CommitSHA ||
				claim.Profile.Version != pin.Version || claim.Profile.CommitSHA != pin.SHA || !reflect.DeepEqual(claim.Profile.Definition, profiles[1].Definition) {
				t.Fatalf("new Execution did not pin current assignment: saved=%+v response=%+v", pin, claim.Profile)
			}
			if mode == "fresh" {
				if claim.Previous != nil {
					t.Fatalf("fresh previous=%+v", claim.Previous)
				}
			} else if claim.Previous == nil || claim.Previous.WorktreePath == nil || *claim.Previous.WorktreePath != path ||
				claim.Previous.BranchName == nil || *claim.Previous.BranchName != branch || claim.Previous.PRNumber == nil || *claim.Previous.PRNumber != 12 {
				t.Fatalf("previous=%+v", claim.Previous)
			}
			var savedStatus, resume string
			if err := f.db.QueryRow(`SELECT status, resume_point FROM executions WHERE id = ?`, nextID).Scan(&savedStatus, &resume); err != nil || savedStatus != "claimed" || resume != "gate3" {
				t.Fatalf("saved status=%s resume=%s err=%v", savedStatus, resume, err)
			}
		})
	}
}

func TestClaimAuthenticationRejectionsLeavePendingTaskUnchanged(t *testing.T) {
	f, token, _ := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "auth-claim")
	for _, tc := range []struct {
		path   string
		owner  bool
		token  string
		status int
		code   Code
	}{
		{"/api/runners/claim-runner/claim", false, "", 401, Unauthenticated},
		{"/api/runners/claim-runner/claim", true, "", 403, Forbidden},
		{"/api/runners/mac-mini/claim", false, token, 403, Forbidden},
	} {
		status, body := f.request(t, "POST", tc.path, tc.owner, f.csrf, func(r *http.Request) {
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
		})
		requireProjectError(t, status, body, tc.status, tc.code, "", "")
		assertUnclaimed(t, f, created.ID)
	}
}
