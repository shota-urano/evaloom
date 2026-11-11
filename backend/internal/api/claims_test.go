package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const claimWorkspaceRoot = "/workspaces/claim-runner"

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
		requireAssignment(t, status, body, 200)
	}
	return f, token, profiles
}

// enableDirectRouting gives harness-a a default version and makes runner the
// Runner of direct routings and route proposals (spec 02 Q111, Q113).
func enableDirectRouting(t *testing.T, f *authFixture, profileID, runner string) {
	t.Helper()
	status, body := projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"defaultProfileId": profileID})
	requireHarness(t, status, body, 200)
	if _, err := f.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('routing.defaultRunnerId', ?, '2026-01-01T00:00:00Z')
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, `"`+runner+`"`); err != nil {
		t.Fatal(err)
	}
}

func createClaimTask(t *testing.T, f *authFixture, project, key string) model.Task {
	t.Helper()
	input := manualTaskInput()
	input["projectId"] = project
	status, body := taskRequest(t, f, key, input, nil)
	return requireTask(t, status, body, 201)
}

func createDirectTask(t *testing.T, f *authFixture, key string) model.Task {
	t.Helper()
	input := manualTaskInput()
	delete(input, "projectId")
	input["harnessId"] = "harness-a"
	status, body := taskRequest(t, f, key, input, nil)
	return requireTask(t, status, body, 201)
}

// insertRouteProposal adds a routing task with a queued proposal (spec 04 3.7).
func insertRouteProposal(t *testing.T, f *authFixture, id, taskID, createdAt string) {
	t.Helper()
	if _, err := f.db.Exec(`INSERT INTO tasks (id, source, title, body, acceptance_criteria, collaboration_mode, status,
		idempotency_key, created_at, updated_at) VALUES (?, 'linear', 'Route me', 'Proposal body', '', 'solo', 'routing', ?, ?, ?)`,
		taskID, "linear:"+id+":1", createdAt, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO route_proposals (id, task_id, trigger, seen_labels, candidates, status, created_at)
		VALUES (?, ?, 'no_label', '[]', ?, 'queued', ?)`, id, taskID,
		`[{"label":"route:project-a","kind":"project","id":"project-a","description":"Project A"}]`, createdAt); err != nil {
		t.Fatal(err)
	}
}

func setExecutionCreatedAt(t *testing.T, f *authFixture, tasks ...model.Task) {
	t.Helper()
	for i, created := range tasks {
		if _, err := f.db.Exec(`UPDATE executions SET created_at = ? WHERE task_id = ?`,
			fmt.Sprintf("2026-10-%02dT00:00:00Z", i+1), created.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func claimRequest(t *testing.T, f *authFixture, token, body string) (int, []byte) {
	t.Helper()
	return f.request(t, "POST", "/api/runners/claim-runner/claim", false, "", func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Body = io.NopCloser(strings.NewReader(body))
		r.ContentLength = int64(len(body))
	})
}

func requestClaim(t *testing.T, f *authFixture, token string) (int, []byte) {
	t.Helper()
	return claimRequest(t, f, token, `{"workspaceRoot":"`+claimWorkspaceRoot+`"}`)
}

func claimJobKind(t *testing.T, status int, body []byte) string {
	t.Helper()
	var reply struct {
		Job *struct {
			Kind string `json:"kind"`
		} `json:"job"`
	}
	if status != 200 || json.Unmarshal(body, &reply) != nil {
		t.Fatalf("claim status=%d body=%s", status, body)
	}
	if reply.Job == nil {
		return ""
	}
	return reply.Job.Kind
}

func requireClaim(t *testing.T, status int, body []byte) store.ExecutionJob {
	t.Helper()
	var reply struct {
		Job store.ExecutionJob `json:"job"`
	}
	if claimJobKind(t, status, body) != "execution" || json.Unmarshal(body, &reply) != nil || reply.Job.Execution.ID == "" {
		t.Fatalf("claim status=%d body=%s", status, body)
	}
	return reply.Job
}

func requireNoJob(t *testing.T, status int, body []byte) {
	t.Helper()
	if status != 200 || strings.TrimSpace(string(body)) != `{"job":null}` {
		t.Fatalf("claim status=%d body=%s; want no job", status, body)
	}
}

func setExecutionState(t *testing.T, f *authFixture, executionID, status string) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE executions SET status = ? WHERE id = ?`, status, executionID); err != nil {
		t.Fatal(err)
	}
}

// capacity returns running work (Executions and proposals) and the most
// active Executions on one route (Project, or direct routing to a harness).
func capacity(t *testing.T, f *authFixture) (int, int) {
	t.Helper()
	var active, perRoute int
	err := f.db.QueryRow(`SELECT
		(SELECT count(*) FROM executions WHERE status IN ('claimed','preparing','working','checking','publishing')) +
		(SELECT count(*) FROM route_proposals WHERE status = 'running'),
		(SELECT coalesce(max(n), 0) FROM (SELECT count(*) n FROM executions e JOIN tasks t ON t.id = e.task_id
			WHERE e.status IN ('claimed','preparing','working','checking','publishing')
			GROUP BY coalesce(t.project_id, 'harness:' || t.harness_id)))`).Scan(&active, &perRoute)
	if err != nil {
		t.Fatal(err)
	}
	return active, perRoute
}

// spec 04 9 (claim): five pending Executions (two on one Project, two direct
// routings to one harness) and one queued proposal, claimed in parallel.
func TestClaimConcurrentLimitsWithProposalFirst(t *testing.T) {
	f, token, profiles := claimFixture(t)
	enableDirectRouting(t, f, profiles[0].ID, "claim-runner")
	tasks := []model.Task{
		createClaimTask(t, f, "project-a", "parallel-a1"),
		createClaimTask(t, f, "project-a", "parallel-a2"),
		createDirectTask(t, f, "parallel-h1"),
		createDirectTask(t, f, "parallel-h2"),
		createClaimTask(t, f, "project-b", "parallel-b"),
	}
	setExecutionCreatedAt(t, f, tasks...)
	// The proposal is the newest work, yet it is delivered first (Q132).
	insertRouteProposal(t, f, "proposal-1", "task-901", "2026-12-01T00:00:00Z")
	type result struct {
		status int
		body   []byte
	}
	results := make(chan result, 6)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, body := requestClaim(t, f, token)
			results <- result{status, body}
			// Observe capacity during concurrent requests, not only at the end.
			if active, perRoute := capacity(t, f); active > 2 || perRoute > 1 {
				t.Errorf("capacity during claim: active=%d perRoute=%d", active, perRoute)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	kinds := map[string]int{}
	var claimed []string
	for r := range results {
		kind := claimJobKind(t, r.status, r.body)
		kinds[kind]++
		if kind == "execution" {
			claimed = append(claimed, requireClaim(t, r.status, r.body).Execution.TaskID)
		}
	}
	if kinds["route_proposal"] != 1 || kinds["execution"] != 1 || kinds[""] != 4 || claimed[0] != tasks[0].ID {
		t.Fatalf("kinds=%v claimed=%v; want the proposal and %s", kinds, claimed, tasks[0].ID)
	}
	var proposalStatus, proposalRunner string
	if err := f.db.QueryRow(`SELECT status, runner_id FROM route_proposals`).Scan(&proposalStatus, &proposalRunner); err != nil ||
		proposalStatus != "running" || proposalRunner != "claim-runner" {
		t.Fatalf("proposal status=%s runner=%s err=%v", proposalStatus, proposalRunner, err)
	}

	// Free the proposal's slot: the next pick skips project-a (busy) and takes
	// the first direct routing; then the global limit holds.
	if _, err := f.db.Exec(`UPDATE route_proposals SET status = 'proposed'`); err != nil {
		t.Fatal(err)
	}
	status, body := requestClaim(t, f, token)
	direct := requireClaim(t, status, body)
	if direct.Execution.TaskID != tasks[2].ID || direct.Route.Kind != model.RouteHarness {
		t.Fatalf("claimed %+v; want direct routing %s", direct.Execution, tasks[2].ID)
	}
	status, body = requestClaim(t, f, token)
	requireNoJob(t, status, body)

	// waiting_approval releases project-a's slot; the second direct routing
	// still waits for the first one on the same harness.
	var first string
	if err := f.db.QueryRow(`SELECT id FROM executions WHERE task_id = ?`, tasks[0].ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	setExecutionState(t, f, first, "waiting_approval")
	status, body = requestClaim(t, f, token)
	if got := requireClaim(t, status, body); got.Execution.TaskID != tasks[1].ID {
		t.Fatalf("claimed %+v; want %s", got.Execution, tasks[1].ID)
	}
	setExecutionState(t, f, direct.Execution.ID, "blocked")
	status, body = requestClaim(t, f, token)
	if got := requireClaim(t, status, body); got.Execution.TaskID != tasks[3].ID {
		t.Fatalf("claimed %+v; want second direct routing %s", got.Execution, tasks[3].ID)
	}
	if active, perRoute := capacity(t, f); active != 2 || perRoute != 1 {
		t.Fatalf("active=%d perRoute=%d", active, perRoute)
	}
	var events, audits int
	if err := f.db.QueryRow(`SELECT
		(SELECT count(*) FROM execution_events WHERE json_extract(payload, '$.reason') = 'runner_claimed'),
		(SELECT count(*) FROM audit_logs WHERE action = 'execution.claim')`).Scan(&events, &audits); err != nil || events != 4 || audits != 4 {
		t.Fatalf("events=%d audits=%d err=%v", events, audits, err)
	}
}

// spec 04 3.3 step 3: Projects sharing a special harness are separate routes,
// and a direct routing is a route of its own.
func TestClaimSameHarnessDifferentProjectsRunTogether(t *testing.T) {
	f, token, profiles := claimFixture(t)
	enableDirectRouting(t, f, profiles[0].ID, "claim-runner")
	a := createClaimTask(t, f, "project-a", "shared-a")
	b := createClaimTask(t, f, "project-b", "shared-b")
	h := createDirectTask(t, f, "shared-h")
	setExecutionCreatedAt(t, f, a, b, h)
	status, body := requestClaim(t, f, token)
	first := requireClaim(t, status, body)
	status, body = requestClaim(t, f, token)
	second := requireClaim(t, status, body)
	if first.Execution.TaskID != a.ID || second.Execution.TaskID != b.ID ||
		first.Profile.HarnessID != "harness-a" || second.Profile.HarnessID != "harness-a" {
		t.Fatalf("first=%+v second=%+v", first.Execution, second.Execution)
	}
	setExecutionState(t, f, first.Execution.ID, "waiting_approval")
	status, body = requestClaim(t, f, token)
	if got := requireClaim(t, status, body); got.Execution.TaskID != h.ID {
		t.Fatalf("direct routing waited for a Project using the same harness: %+v", got.Execution)
	}
}

type executionPin struct {
	Runner, RouteKind, Project, Harness, Profile, Version, SHA, Contract, Params, Workspace, ClaimedAt string
}

func readExecutionPin(t *testing.T, f *authFixture, id string) executionPin {
	t.Helper()
	var pin executionPin
	err := f.db.QueryRow(`SELECT runner_id, route_kind, coalesce(project_id, ''), harness_id, profile_id, profile_version,
		profile_commit_sha, contract_version, params, workspace_id, claimed_at FROM executions WHERE id = ?`, id).
		Scan(&pin.Runner, &pin.RouteKind, &pin.Project, &pin.Harness, &pin.Profile, &pin.Version, &pin.SHA,
			&pin.Contract, &pin.Params, &pin.Workspace, &pin.ClaimedAt)
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

// spec 04 3.3 step 4, 3.3.1: the claim pins version, params and workspace,
// returns them, and later Project changes do not alter the Execution.
func TestClaimPinsProjectRouteAndLaterChangesPreserveExecution(t *testing.T) {
	f, token, profiles := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "pin")
	// A queued task picks the Project's version at claim, not at creation.
	status, body := postAssignment(t, f, "project-a", "developer", "harness-a", profiles[1].Version)
	requireAssignment(t, status, body, 200)
	status, body = requestClaim(t, f, token)
	job := requireClaim(t, status, body)
	pin := readExecutionPin(t, f, job.Execution.ID)
	want := executionPin{"claim-runner", "project", "project-a", "harness-a", profiles[1].ID, profiles[1].Version,
		profiles[1].CommitSHA, "1", "{}", job.Workspace.ID, pin.ClaimedAt}
	if pin != want || pin.ClaimedAt == "" {
		t.Fatalf("saved pin=%+v want=%+v", pin, want)
	}
	if _, err := ParseTimestamp(pin.ClaimedAt); err != nil {
		t.Fatal(err)
	}
	harness, err := store.GetHarness(context.Background(), f.db, "harness-a")
	if err != nil {
		t.Fatal(err)
	}
	projectID, projectName := "project-a", "Project project-a"
	if job.Kind != "execution" || job.Execution.TaskID != created.ID || job.Execution.Seq != 1 ||
		job.Execution.Mode != model.ModeContinue || job.Execution.ResumeStage != nil ||
		job.Task.ID != created.ID || job.Task.Title != created.Title || job.Task.Body != created.Body ||
		job.Task.AcceptanceCriteria != created.AcceptanceCriteria || job.Task.Source.Kind != model.SourceManual ||
		job.Task.Source.Identifier != nil || job.Task.Source.URL != nil ||
		job.Route.Kind != model.RouteProject || *job.Route.ProjectID != projectID || *job.Route.ProjectName != projectName ||
		string(job.Params) != "{}" ||
		job.Profile.ID != profiles[1].ID || job.Profile.HarnessID != "harness-a" || job.Profile.Version != profiles[1].Version ||
		job.Profile.CommitSHA != profiles[1].CommitSHA || job.Profile.TagName != profiles[1].TagName ||
		job.Profile.RepoPath != harness.RepoPath || !reflect.DeepEqual(job.Profile.Definition, profiles[1].Definition) ||
		job.Workspace.Path != claimWorkspaceRoot+"/"+job.Workspace.ID || !job.Workspace.Created ||
		!reflect.DeepEqual(job.Limits.TimeoutSec, map[model.Stage]int{model.StageWork: 600, model.StageCheck: 60, model.StagePublish: 60}) ||
		job.Previous != nil || job.Resume != nil {
		t.Fatalf("response does not match stored input: %s", body)
	}
	var workspace struct{ task, runner, path, status, creator string }
	if err := f.db.QueryRow(`SELECT task_id, runner_id, path, status, created_by_execution_id FROM task_workspaces WHERE id = ?`, job.Workspace.ID).
		Scan(&workspace.task, &workspace.runner, &workspace.path, &workspace.status, &workspace.creator); err != nil ||
		workspace.task != created.ID || workspace.runner != "claim-runner" || workspace.path != job.Workspace.Path ||
		workspace.status != "active" || workspace.creator != job.Execution.ID {
		t.Fatalf("workspace=%+v err=%v", workspace, err)
	}
	if strings.Contains(string(body), token) || strings.Contains(string(body), "tokenHash") {
		t.Fatal("credentials leaked")
	}
	var event, audit, taskStatus string
	err = f.db.QueryRow(`SELECT payload FROM execution_events WHERE execution_id = ? AND json_extract(payload, '$.reason') = 'runner_claimed'`, job.Execution.ID).Scan(&event)
	if err != nil || event != `{"from":"pending","to":"claimed","reason":"runner_claimed"}` {
		t.Fatalf("event=%s err=%v", event, err)
	}
	err = f.db.QueryRow(`SELECT detail FROM audit_logs WHERE actor = 'runner:claim-runner' AND action = 'execution.claim' AND target_type = 'execution' AND target_id = ?`, job.Execution.ID).Scan(&audit)
	var detail struct {
		ProfileID        string
		ProfileCommitSHA string
		WorkspaceID      string
	}
	if err != nil || json.Unmarshal([]byte(audit), &detail) != nil || detail.ProfileID != pin.Profile || detail.ProfileCommitSHA != pin.SHA || detail.WorkspaceID != pin.Workspace {
		t.Fatalf("audit=%s err=%v", audit, err)
	}
	if err := f.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, created.ID).Scan(&taskStatus); err != nil || taskStatus != "running" {
		t.Fatalf("task status=%s err=%v", taskStatus, err)
	}
	// Change the Project's version and params after the claim.
	status, body = postAssignment(t, f, "project-a", "developer", "harness-a", profiles[0].Version)
	requireAssignment(t, status, body, 200)
	if _, err := f.db.Exec(`UPDATE projects SET params = '{"changed":true}' WHERE id = 'project-a'`); err != nil {
		t.Fatal(err)
	}
	if storedAssignment(t, f, "project-a", "developer").ProfileID != profiles[0].ID || readExecutionPin(t, f, job.Execution.ID) != pin {
		t.Fatal("Project change rewrote the Execution's pinned values")
	}
	status, body = requestClaim(t, f, token)
	requireNoJob(t, status, body)
	if readExecutionPin(t, f, job.Execution.ID) != pin {
		t.Fatal("repeated claim rewrote the Execution's pinned values")
	}
}

// spec 04 3.3 step 4, contract 6.1: a direct routing pins the harness's default
// version and {} params; route.projectId and projectName are null.
func TestClaimPinsDirectRouting(t *testing.T) {
	f, token, profiles := claimFixture(t)
	enableDirectRouting(t, f, profiles[1].ID, "claim-runner")
	created := createDirectTask(t, f, "direct-pin")
	status, body := requestClaim(t, f, token)
	job := requireClaim(t, status, body)
	pin := readExecutionPin(t, f, job.Execution.ID)
	if pin.RouteKind != "harness" || pin.Project != "" || pin.Harness != "harness-a" || pin.Profile != profiles[1].ID ||
		pin.SHA != profiles[1].CommitSHA || pin.Params != "{}" || pin.Workspace != job.Workspace.ID {
		t.Fatalf("pin=%+v", pin)
	}
	if job.Execution.TaskID != created.ID || job.Route.Kind != model.RouteHarness || job.Route.ProjectID != nil ||
		job.Route.ProjectName != nil || string(job.Params) != "{}" || job.Profile.ID != profiles[1].ID {
		t.Fatalf("job=%s", body)
	}
	var raw struct {
		Job struct {
			Route map[string]any `json:"route"`
		} `json:"job"`
	}
	if json.Unmarshal(body, &raw) != nil || !reflect.DeepEqual(raw.Job.Route, map[string]any{"kind": "harness", "projectId": nil, "projectName": nil}) {
		t.Fatalf("route=%v", raw.Job.Route)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"defaultProfileId": profiles[0].ID})
	requireHarness(t, status, body, 200)
	if readExecutionPin(t, f, job.Execution.ID) != pin {
		t.Fatal("default version change rewrote the Execution's pinned values")
	}
}

// spec 04 3.3.1: only the listed items; no worktree, branch or PR items.
func TestClaimResponseHasNoDevelopmentItems(t *testing.T) {
	f, token, _ := claimFixture(t)
	createClaimTask(t, f, "project-a", "shape")
	status, body := requestClaim(t, f, token)
	requireClaim(t, status, body)
	var raw struct {
		Job map[string]json.RawMessage `json:"job"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for key := range raw.Job {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := []string{"execution", "kind", "limits", "params", "previous", "profile", "resume", "route", "task", "workspace"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("job keys=%v want=%v", keys, want)
	}
	lower := strings.ToLower(string(body))
	for _, word := range []string{"worktree", "branch", "pullrequest", "pr_", "prnumber", "prurl", "testcommand", "\"project\":"} {
		if strings.Contains(lower, word) {
			t.Fatalf("claim response contains %q: %s", word, body)
		}
	}
}

func TestClaimCountsAllActiveStatesAndReleasesBlockedAndTerminalSlots(t *testing.T) {
	f, token, _ := claimFixture(t)
	a := createClaimTask(t, f, "project-a", "active-a")
	aPending := createClaimTask(t, f, "project-a", "pending-a")
	b := createClaimTask(t, f, "project-b", "pending-b")
	setExecutionCreatedAt(t, f, a, aPending, b)
	status, body := requestClaim(t, f, token)
	first := requireClaim(t, status, body)
	for _, state := range []string{"claimed", "preparing", "working", "checking", "publishing"} {
		t.Run(state, func(t *testing.T) {
			setExecutionState(t, f, first.Execution.ID, state)
			status, body := requestClaim(t, f, token)
			claim := requireClaim(t, status, body)
			if claim.Execution.TaskID != b.ID {
				t.Fatalf("active Project was not skipped: %+v", claim.Execution)
			}
			// Global capacity includes active work assigned to another Runner.
			if _, err := f.db.Exec(`UPDATE executions SET runner_id = 'mac-mini', status = ? WHERE task_id = ?`, state, b.ID); err != nil {
				t.Fatal(err)
			}
			status, body = requestClaim(t, f, token)
			requireNoJob(t, status, body)
			// A running proposal also takes a global slot.
			if _, err := f.db.Exec(`UPDATE executions SET status = 'waiting_approval' WHERE task_id = ?`, b.ID); err != nil {
				t.Fatal(err)
			}
			insertRouteProposal(t, f, "proposal-"+state, "task-902", "2026-01-01T00:00:00Z")
			if _, err := f.db.Exec(`UPDATE route_proposals SET status = 'running'`); err != nil {
				t.Fatal(err)
			}
			status, body = requestClaim(t, f, token)
			requireNoJob(t, status, body)
			if _, err := f.db.Exec(`DELETE FROM route_proposals; DELETE FROM tasks WHERE status = 'routing'`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE executions SET status = 'pending', runner_id = NULL, profile_id = NULL WHERE task_id = ?`, b.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE tasks SET status = 'queued' WHERE id = ?`, b.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, state := range []string{"blocked", "waiting_approval", "succeeded", "failed", "interrupted", "cancelled", "timed_out"} {
		t.Run(state, func(t *testing.T) {
			setExecutionState(t, f, first.Execution.ID, state)
			status, body := requestClaim(t, f, token)
			claim := requireClaim(t, status, body)
			if *claim.Route.ProjectID != "project-a" || claim.Execution.ID == first.Execution.ID {
				t.Fatalf("released Project was not claimable: %+v", claim.Execution)
			}
			setExecutionState(t, f, claim.Execution.ID, "pending")
			if _, err := f.db.Exec(`UPDATE tasks SET status = 'queued' WHERE id = ?`, claim.Execution.TaskID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// spec 04 3.3, 4: waiting_approval and blocked Executions occupy neither the
// global limit of 2 nor their route's slot.
func TestClaimDoesNotCountWaitingApprovalOrBlocked(t *testing.T) {
	f, token, _ := claimFixture(t)
	claimNext := func() store.ExecutionJob {
		t.Helper()
		status, body := requestClaim(t, f, token)
		return requireClaim(t, status, body)
	}
	waitingA := createClaimTask(t, f, "project-a", "waiting-a")
	blockedB := createClaimTask(t, f, "project-b", "blocked-b")
	pendingA := createClaimTask(t, f, "project-a", "pending-a")
	pendingB := createClaimTask(t, f, "project-b", "pending-b")
	pendingC := createClaimTask(t, f, "project-a", "pending-c")
	setExecutionCreatedAt(t, f, waitingA, blockedB, pendingA, pendingB, pendingC)
	for _, want := range []struct{ task, status string }{{waitingA.ID, "waiting_approval"}, {blockedB.ID, "blocked"}} {
		claim := claimNext()
		if claim.Execution.TaskID != want.task {
			t.Fatalf("claimed %s; want %s", claim.Execution.TaskID, want.task)
		}
		setExecutionState(t, f, claim.Execution.ID, want.status)
		if _, err := f.db.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, want.status, want.task); err != nil {
			t.Fatal(err)
		}
	}
	// If either state counted, project-a (and then the global limit) would block.
	if got := claimNext(); got.Execution.TaskID != pendingA.ID {
		t.Fatalf("project-a with a waiting_approval Execution was not claimable: %+v", got.Execution)
	}
	if got := claimNext(); got.Execution.TaskID != pendingB.ID {
		t.Fatalf("project-b with a blocked Execution was not claimable: %+v", got.Execution)
	}
	status, body := requestClaim(t, f, token)
	requireNoJob(t, status, body)
	var pendingCStatus string
	if err := f.db.QueryRow(`SELECT status FROM executions WHERE task_id = ?`, pendingC.ID).Scan(&pendingCStatus); err != nil || pendingCStatus != "pending" {
		t.Fatalf("pending-c status=%q err=%v", pendingCStatus, err)
	}
}

// spec 04 3.3 step 2, 3 (Q113): Project tasks go to projects.runner_id; direct
// routings and proposals only to routing.defaultRunnerId.
func TestClaimFiltersRouteRunner(t *testing.T) {
	f, token, profiles := claimFixture(t)
	enableDirectRouting(t, f, profiles[0].ID, "mac-mini")
	a := createClaimTask(t, f, "project-a", "foreign")
	h := createDirectTask(t, f, "foreign-direct")
	b := createClaimTask(t, f, "project-b", "own")
	insertRouteProposal(t, f, "proposal-foreign", "task-903", "2026-01-01T00:00:00Z")
	if _, err := f.db.Exec(`UPDATE projects SET runner_id = 'mac-mini' WHERE id = 'project-a'`); err != nil {
		t.Fatal(err)
	}
	status, body := requestClaim(t, f, token)
	if claim := requireClaim(t, status, body); claim.Execution.TaskID != b.ID {
		t.Fatalf("claim=%+v", claim.Execution)
	}
	status, body = requestClaim(t, f, token)
	requireNoJob(t, status, body)
	for _, id := range []string{a.ID, h.ID} {
		assertUnclaimed(t, f, id)
	}
	var proposal string
	if err := f.db.QueryRow(`SELECT status FROM route_proposals`).Scan(&proposal); err != nil || proposal != "queued" {
		t.Fatalf("proposal=%s err=%v", proposal, err)
	}
}

// spec 04 3.3 (config_error): a candidate whose pinned inputs are unusable
// becomes failed (config_error) with a notification, and the next candidate
// is examined in the same claim.
func TestClaimConfigErrorsFailCandidateAndContinue(t *testing.T) {
	for _, tc := range []struct {
		name        string
		breakConfig func(t *testing.T, f *authFixture, profiles []model.HarnessProfile) model.Task
	}{
		{"direct routing without default version", func(t *testing.T, f *authFixture, profiles []model.HarnessProfile) model.Task {
			enableDirectRouting(t, f, profiles[0].ID, "claim-runner")
			created := createDirectTask(t, f, "no-default")
			if _, err := f.db.Exec(`UPDATE harnesses SET default_profile_id = NULL WHERE id = 'harness-a'`); err != nil {
				t.Fatal(err)
			}
			return created
		}},
		{"required acceptance criteria after version change", func(t *testing.T, f *authFixture, profiles []model.HarnessProfile) model.Task {
			input := manualTaskInput()
			input["projectId"] = "project-a"
			input["acceptanceCriteria"] = ""
			status, body := taskRequest(t, f, "empty-criteria", input, nil)
			created := requireTask(t, status, body, 201)
			if _, err := f.db.Exec(`UPDATE harness_profiles SET definition = json_set(definition, '$.acceptanceCriteria', 'required') WHERE id = ?`, profiles[0].ID); err != nil {
				t.Fatal(err)
			}
			return created
		}},
		{"params do not match params.schema", func(t *testing.T, f *authFixture, profiles []model.HarnessProfile) model.Task {
			created := createClaimTask(t, f, "project-a", "bad-params")
			if _, err := f.db.Exec(`UPDATE harness_profiles SET definition = json_set(definition, '$.params', json('{"schema":"evaloom/params.schema.json"}')) WHERE id = ?`, profiles[0].ID); err != nil {
				t.Fatal(err)
			}
			// The fixture's schema is {"type":"object"}.
			if _, err := f.db.Exec(`UPDATE projects SET params = '[]' WHERE id = 'project-a'`); err != nil {
				t.Fatal(err)
			}
			return created
		}},
		{"timeoutSec above lowered limit", func(t *testing.T, f *authFixture, profiles []model.HarnessProfile) model.Task {
			created := createClaimTask(t, f, "project-a", "timeout")
			if _, err := f.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('limits.stageTimeoutMaxSec', ?, '2026-01-01T00:00:00Z')`,
				`{"prepare":1800,"work":599,"check":3600,"publish":1800}`); err != nil {
				t.Fatal(err)
			}
			return created
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, profiles := claimFixture(t)
			broken := tc.breakConfig(t, f, profiles)
			good := createClaimTask(t, f, "project-b", "good")
			if _, err := f.db.Exec(`UPDATE projects SET profile_id = ? WHERE id = 'project-b'`, profiles[1].ID); err != nil {
				t.Fatal(err)
			}
			if tc.name == "timeoutSec above lowered limit" {
				if _, err := f.db.Exec(`UPDATE harness_profiles SET definition = json_set(definition, '$.stages.work.timeoutSec', 500) WHERE id = ?`, profiles[1].ID); err != nil {
					t.Fatal(err)
				}
			}
			setExecutionCreatedAt(t, f, broken, good)
			status, body := requestClaim(t, f, token)
			if got := requireClaim(t, status, body); got.Execution.TaskID != good.ID {
				t.Fatalf("claimed %+v; want next candidate %s", got.Execution, good.ID)
			}
			var execStatus, errorClass, taskStatus string
			var workspaces, notifications, failedEvents int
			if err := f.db.QueryRow(`SELECT e.status, coalesce(e.error_class, ''), t.status,
				(SELECT count(*) FROM task_workspaces w WHERE w.task_id = t.id),
				(SELECT count(*) FROM notification_requests n WHERE n.execution_id = e.id AND n.status = 'failed'),
				(SELECT count(*) FROM execution_events v WHERE v.execution_id = e.id AND json_extract(v.payload, '$.reason') = 'config_error')
				FROM executions e JOIN tasks t ON t.id = e.task_id WHERE t.id = ?`, broken.ID).
				Scan(&execStatus, &errorClass, &taskStatus, &workspaces, &notifications, &failedEvents); err != nil {
				t.Fatal(err)
			}
			if execStatus != "failed" || errorClass != "config_error" || taskStatus != "failed" || workspaces != 0 || notifications != 1 || failedEvents != 1 {
				t.Fatalf("execution=%s class=%s task=%s workspaces=%d notifications=%d events=%d",
					execStatus, errorClass, taskStatus, workspaces, notifications, failedEvents)
			}
		})
	}
}

// spec 04 3.3 step 4: mode=continue inherits the task's latest active or
// retained workspace; mode=fresh creates a new row.
func TestClaimWorkspaceContinueAndFresh(t *testing.T) {
	f, token, _ := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "workspace")
	status, body := requestClaim(t, f, token)
	first := requireClaim(t, status, body)
	for i, mode := range []string{"continue", "fresh"} {
		setExecutionState(t, f, first.Execution.ID, "failed")
		if _, err := f.db.Exec(`UPDATE tasks SET status = 'queued' WHERE id = ?`, created.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`UPDATE task_workspaces SET status = 'retained'`); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`INSERT INTO executions (id, task_id, seq, mode, status, created_at) VALUES (?, ?, ?, ?, 'pending', '2027-01-01T00:00:00Z')`,
			fmt.Sprintf("01JRETRY%018d", i), created.ID, i+2, mode); err != nil {
			t.Fatal(err)
		}
		status, body = requestClaim(t, f, token)
		job := requireClaim(t, status, body)
		if job.Previous == nil || job.Previous.ExecutionID != first.Execution.ID || len(job.Previous.Outputs) != 0 {
			t.Fatalf("previous=%+v", job.Previous)
		}
		inherited := job.Workspace.ID == first.Workspace.ID && !job.Workspace.Created && job.Workspace.Path == first.Workspace.Path
		if (mode == "continue") != inherited || (mode == "fresh" && !job.Workspace.Created) {
			t.Fatalf("mode=%s workspace=%+v first=%+v", mode, job.Workspace, first.Workspace)
		}
		first = job
	}
}

func assertUnclaimed(t *testing.T, f *authFixture, taskID string) {
	t.Helper()
	var untouched, workspaces int
	err := f.db.QueryRow(`SELECT count(*) FROM executions e JOIN tasks t ON t.id = e.task_id
		WHERE t.id = ? AND t.status = 'queued' AND e.status = 'pending' AND e.runner_id IS NULL
		AND e.route_kind IS NULL AND e.harness_id IS NULL AND e.profile_id IS NULL AND e.params IS NULL
		AND e.profile_version IS NULL AND e.profile_commit_sha IS NULL AND e.workspace_id IS NULL AND e.claimed_at IS NULL`, taskID).Scan(&untouched)
	if err != nil || untouched != 1 {
		t.Fatalf("unclaimed=%d err=%v", untouched, err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM task_workspaces WHERE task_id = ?`, taskID).Scan(&workspaces); err != nil || workspaces != 0 {
		t.Fatalf("workspaces=%d err=%v", workspaces, err)
	}
}

func assertNoClaimRecords(t *testing.T, f *authFixture) {
	t.Helper()
	var events, audits int
	if err := f.db.QueryRow(`SELECT
		(SELECT count(*) FROM execution_events WHERE json_extract(payload, '$.reason') = 'runner_claimed'),
		(SELECT count(*) FROM audit_logs WHERE action = 'execution.claim')`).Scan(&events, &audits); err != nil {
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
		`BEFORE INSERT ON task_workspaces`,
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
		assertNoClaimRecords(t, f)
		if _, err := f.db.Exec(`DROP TRIGGER reject_claim`); err != nil {
			t.Fatal(err)
		}
	}
	status, body := requestClaim(t, f, token)
	requireClaim(t, status, body)
}

func TestClaimStateConflictLeavesNoPins(t *testing.T) {
	f, token, _ := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "preconditions")
	if _, err := f.db.Exec(`UPDATE tasks SET status = 'cancelled' WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	status, body := requestClaim(t, f, token)
	requireProjectError(t, status, body, 409, Conflict, "", "")
	if _, err := f.db.Exec(`UPDATE tasks SET status = 'queued' WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	assertUnclaimed(t, f, created.ID)
	assertNoClaimRecords(t, f)
}

// spec 04 3.3 (Q134): the request carries the Runner's absolute workspaceRoot.
func TestClaimRequiresAbsoluteWorkspaceRoot(t *testing.T) {
	f, token, _ := claimFixture(t)
	created := createClaimTask(t, f, "project-a", "root")
	for _, tc := range []struct{ body, field, reason string }{
		{``, "body", "invalid_json"},
		{`{}`, "workspaceRoot", "required"},
		{`{"workspaceRoot":""}`, "workspaceRoot", "invalid_format"},
		{`{"workspaceRoot":"relative/root"}`, "workspaceRoot", "invalid_format"},
		{`{"workspaceRoot":"/root/../escape"}`, "workspaceRoot", "invalid_format"},
		{`{"workspaceRoot":"/root","worktreeRoot":"/x"}`, "body", "invalid_json"},
	} {
		status, body := claimRequest(t, f, token, tc.body)
		requireProjectError(t, status, body, 400, ValidationFailed, tc.field, tc.reason)
		assertUnclaimed(t, f, created.ID)
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
			r.Body = io.NopCloser(strings.NewReader(`{"workspaceRoot":"/workspaces"}`))
		})
		requireProjectError(t, status, body, tc.status, tc.code, "", "")
		assertUnclaimed(t, f, created.ID)
	}
}
