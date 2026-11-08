package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// acceptancePullRequest is the test GitHub's PR 1 backed by the fixture's
// bare remote. The head SHA it reports is the remote's actual task branch, so
// a commit pushed after approval is what GitHub would report. A merge request
// is checked against that head like GitHub's sha parameter (409 on mismatch)
// and, when accepted, creates a real merge commit on the remote main. Every
// merge request is recorded, accepted or not.
type acceptancePullRequest struct {
	remote, branch string
	merges         []map[string]string
	mergeCommit    string
	// moveHeadOnMerge pushes another commit to the PR right before GitHub
	// checks the sha parameter of the merge request.
	moveHeadOnMerge bool
}

func acceptanceGit(path string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", path, "-c", "core.hooksPath=/dev/null", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
	body, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %s %w", args, body, err)
	}
	return strings.TrimSpace(string(body)), nil
}

// pushAfterApproval adds a commit to the PR's head branch on the remote, as a
// later push to the PR would.
func (p *acceptancePullRequest) pushAfterApproval() (string, error) {
	head, err := acceptanceGit(p.remote, "rev-parse", "refs/heads/"+p.branch)
	if err != nil {
		return "", err
	}
	next, err := acceptanceGit(p.remote, "commit-tree", "-p", head, "-m", "pushed after approval", head+"^{tree}")
	if err != nil {
		return "", err
	}
	_, err = acceptanceGit(p.remote, "update-ref", "refs/heads/"+p.branch, next, head)
	return next, err
}

// serve answers GET and PUT .../pulls/1(/merge). It runs under f.mu.
func (p *acceptancePullRequest) serve(f *productionPipelineFixture, w http.ResponseWriter, r *http.Request) bool {
	fail := func(err error) bool {
		f.t.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return true
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
		head, err := acceptanceGit(p.remote, "rev-parse", "refs/heads/"+p.branch)
		if err != nil {
			return fail(err)
		}
		pr := map[string]any{"number": 1, "html_url": "https://github.com/owner/repo/pull/1", "state": "open", "merged": p.mergeCommit != "",
			"head": map[string]any{"sha": head, "ref": p.branch}, "base": map[string]string{"ref": "main"}}
		if p.mergeCommit != "" {
			pr["state"], pr["merge_commit_sha"] = "closed", p.mergeCommit
		}
		_ = json.NewEncoder(w).Encode(pr)
		return true
	case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/pulls/1/merge":
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return fail(err)
		}
		p.merges = append(p.merges, input)
		if p.moveHeadOnMerge {
			if _, err := p.pushAfterApproval(); err != nil {
				return fail(err)
			}
		}
		head, err := acceptanceGit(p.remote, "rev-parse", "refs/heads/"+p.branch)
		if err != nil {
			return fail(err)
		}
		if p.mergeCommit != "" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			io.WriteString(w, `{"message":"Pull Request is not mergeable"}`)
			return true
		}
		if input["sha"] != head {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"message":"Head branch was modified. Review and try the merge again."}`)
			return true
		}
		main, err := acceptanceGit(p.remote, "rev-parse", "refs/heads/main")
		if err != nil {
			return fail(err)
		}
		merge, err := acceptanceGit(p.remote, "commit-tree", "-p", main, "-p", head, "-m", "Merge pull request #1", head+"^{tree}")
		if err != nil {
			return fail(err)
		}
		if _, err := acceptanceGit(p.remote, "update-ref", "refs/heads/main", merge, main); err != nil {
			return fail(err)
		}
		p.mergeCommit = merge
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": merge, "merged": true, "message": "Pull Request successfully merged"})
		return true
	}
	return false
}

// approvalAcceptance is spec 17 9章's acceptance test of conditions 21 and 22
// on top of spec 14's test tools: the production pipeline (real Runner,
// Control Plane handlers, SQLite, bare remote) publishes PR 1 of a
// require_approval Project, and the Control Plane opens the approval.
type approvalAcceptance struct {
	*productionPipelineFixture
	pr       *acceptancePullRequest
	pushed   string // the published head (content_version)
	approval string
}

func newApprovalAcceptance(t *testing.T) *approvalAcceptance {
	t.Helper()
	f := newProductionPipelineFixture(t, "success")
	if err := f.execute(t); err != nil {
		t.Fatal(err)
	}
	// require_approval: the PR waits for approval with one pending row.
	f.assertSucceeded(t)
	a := &approvalAcceptance{productionPipelineFixture: f}
	var branch string
	if err := f.db.QueryRow(`SELECT branch_name,pushed_sha FROM executions WHERE id=?`, logExecution).Scan(&branch, &a.pushed); err != nil {
		t.Fatal(err)
	}
	a.pr = &acceptancePullRequest{remote: f.remote, branch: branch}
	f.mu.Lock()
	f.pullRequest = a.pr
	f.mu.Unlock()
	var page struct {
		Items []struct {
			ID, ContentVersion, Status, Operation string
		}
	}
	a.owner(http.MethodGet, "/api/approvals?status=pending", nil, http.StatusOK, &page)
	if len(page.Items) != 1 || page.Items[0].ContentVersion != a.pushed || page.Items[0].Operation != "merge_pull_request" {
		t.Fatalf("pending approvals = %+v; want one for %s", page.Items, a.pushed)
	}
	a.approval = page.Items[0].ID
	if main := gate2Git(t, f.remote, "rev-parse", "refs/heads/main"); main != f.initialMain {
		t.Fatalf("remote main %s moved before approval", main)
	}
	return a
}

// owner calls a 本人用 API with the owner's session, as the management UI does.
func (a *approvalAcceptance) owner(method, path string, input any, want int, output any) {
	t := a.t
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, a.server.URL+"/api/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(a.cookie)
	response, err := a.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var session struct{ CSRFToken string }
	err = json.NewDecoder(response.Body).Decode(&session)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || session.CSRFToken == "" {
		t.Fatalf("session status=%d err=%v", response.StatusCode, err)
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	request, err = http.NewRequest(method, a.server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(a.cookie)
	if method != http.MethodGet {
		request.Header.Set("Origin", a.server.URL)
		request.Header.Set("X-CSRF-Token", session.CSRFToken)
		request.Header.Set("Content-Type", "application/json")
	}
	response, err = a.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != want {
		t.Fatalf("%s %s = %d %s %v; want %d", method, path, response.StatusCode, data, err, want)
	}
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			t.Fatal(err)
		}
	}
}

// approve sends the owner's approval with the content_version on screen.
func (a *approvalAcceptance) approve(ttlSec int) {
	a.t.Helper()
	a.owner(http.MethodPost, "/api/approvals/"+a.approval+"/decision",
		map[string]any{"decision": "approve", "contentVersion": a.pushed, "ttlSec": ttlSec}, http.StatusOK, nil)
}

// claimOperation polls the real claim endpoint once.
func (a *approvalAcceptance) claimOperation() *Claim {
	a.t.Helper()
	claim, err := a.process.claim(context.Background())
	if err != nil {
		a.t.Fatal(err)
	}
	return claim
}

// mergeOnce claims the approved operation and lets the Runner's Git
// Publisher perform it against the test GitHub.
func (a *approvalAcceptance) mergeOnce() Claim {
	a.t.Helper()
	claim := a.claimOperation()
	if claim == nil || claim.Operation == nil || claim.Operation.ApprovalID != a.approval || claim.Operation.ContentVersion != a.pushed {
		a.t.Fatalf("claim = %+v; want the approved merge", claim)
	}
	if err := a.process.ExecuteOperation(context.Background(), context.Background(), a.config, *claim); err != nil {
		a.t.Fatal(err)
	}
	return *claim
}

// merges is what the test GitHub received: every merge request, and the
// merge commit if one was made.
func (a *approvalAcceptance) merges() ([]map[string]string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]string(nil), a.pr.merges...), a.pr.mergeCommit
}

func (a *approvalAcceptance) state() (execution, task, approval, reason string) {
	a.t.Helper()
	if err := a.db.QueryRow(`SELECT e.status, t.status, a.status, coalesce(a.invalidated_reason,'')
		FROM approvals a JOIN executions e ON e.id=a.execution_id JOIN tasks t ON t.id=e.task_id WHERE a.id=?`, a.approval).
		Scan(&execution, &task, &approval, &reason); err != nil {
		a.t.Fatal(err)
	}
	return
}

// assertNotMerged checks from outside Evaloom that nothing was merged: the
// test GitHub received no merge request and the remote main is unchanged.
func (a *approvalAcceptance) assertNotMerged() {
	a.t.Helper()
	if merges, commit := a.merges(); len(merges) != 0 || commit != "" {
		a.t.Fatalf("merge requests = %v merge commit %q; want none", merges, commit)
	}
	if main := gate2Git(a.t, a.remote, "rev-parse", "refs/heads/main"); main != a.initialMain {
		a.t.Fatalf("remote main moved to %s", main)
	}
}

// Spec 17 9章, conditions 21 and 22 (spec 14 test group). Each case observes
// the merge requests that reach the test GitHub API and the remote main.
func TestApprovalAcceptance(t *testing.T) {
	// Condition 21: a Project that keeps PR merges under approval does not
	// merge without the owner's approval.
	t.Run("no_merge_without_approval", func(t *testing.T) {
		a := newApprovalAcceptance(t)
		// The Runner keeps polling: no approved operation is handed out.
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if err := a.process.Run(ctx, func(context.Context, Claim) { t.Error("claimed work without approval") }, func(err error) { t.Error(err) }); err != nil {
			t.Fatal(err)
		}
		// Even a Runner that asks to merge the pending approval is refused at
		// consume and sends nothing to GitHub.
		var claim Claim
		claim.Execution.ID, claim.Execution.TaskID = logExecution, "task-001"
		claim.Project.GitRemoteURL = readURL
		var target string
		if err := a.db.QueryRow(`SELECT target FROM approvals WHERE id=?`, a.approval).Scan(&target); err != nil {
			t.Fatal(err)
		}
		claim.Operation = &ClaimOperation{ApprovalID: a.approval, Operation: "merge_pull_request", Target: json.RawMessage(target), ContentVersion: a.pushed}
		if err := a.process.ExecuteOperation(context.Background(), context.Background(), a.config, claim); err != nil {
			t.Fatal(err)
		}
		a.assertNotMerged()
		if execution, task, approval, _ := a.state(); execution != "waiting_approval" || task != "waiting_approval" || approval != "pending" {
			t.Fatalf("state = %s/%s/%s; want waiting_approval/waiting_approval/pending", execution, task, approval)
		}
		var reason string
		if err := a.db.QueryRow(`SELECT json_extract(detail,'$.reason') FROM audit_logs WHERE actor='runner:mac-mini' AND action='approval.consume' AND target_id=?`,
			a.approval).Scan(&reason); err != nil || reason != "not_approved" {
			t.Fatalf("consume audit reason %q %v; want not_approved", reason, err)
		}
		// Positive control: the same setup merges once the owner approves.
		a.approve(3600)
		a.mergeOnce()
		if merges, commit := a.merges(); len(merges) != 1 || merges[0]["sha"] != a.pushed || commit == "" {
			t.Fatalf("merge requests after approval = %v %q; want one with sha %s", merges, commit, a.pushed)
		}
	})

	// Condition 22: after approval the PR head changes, so the approval no
	// longer matches the content and nothing is merged.
	t.Run("head_changed_after_approval", func(t *testing.T) {
		a := newApprovalAcceptance(t)
		a.approve(3600)
		a.mu.Lock()
		moved, err := a.pr.pushAfterApproval()
		a.mu.Unlock()
		if err != nil || moved == a.pushed {
			t.Fatalf("push after approval %s %v", moved, err)
		}
		a.mergeOnce()
		a.assertNotMerged()
		if execution, task, approval, reason := a.state(); execution != "waiting_approval" || task != "waiting_approval" || approval != "invalidated" || reason != "content_changed" {
			t.Fatalf("state = %s/%s/%s/%s; want waiting_approval/waiting_approval/invalidated/content_changed", execution, task, approval, reason)
		}
		// The invalidated approval is never handed out again.
		if claim := a.claimOperation(); claim != nil {
			t.Fatalf("claim after content change = %+v; want none", claim)
		}
		a.assertNotMerged()
	})

	// Condition 22, the window between consume and the merge request: GitHub
	// refuses the sha parameter, so the changed head is not merged.
	t.Run("head_changed_before_merge_request", func(t *testing.T) {
		a := newApprovalAcceptance(t)
		a.approve(3600)
		a.mu.Lock()
		a.pr.moveHeadOnMerge = true
		a.mu.Unlock()
		a.mergeOnce()
		merges, commit := a.merges()
		if len(merges) != 1 || merges[0]["sha"] != a.pushed || commit != "" {
			t.Fatalf("merge requests = %v merge commit %q; want one refused request with sha %s", merges, commit, a.pushed)
		}
		if main := gate2Git(t, a.remote, "rev-parse", "refs/heads/main"); main != a.initialMain {
			t.Fatalf("remote main moved to %s", main)
		}
		if execution, task, approval, reason := a.state(); execution != "failed" || task != "failed" || approval != "invalidated" || reason != "content_changed" {
			t.Fatalf("state = %s/%s/%s/%s; want failed/failed/invalidated/content_changed", execution, task, approval, reason)
		}
	})

	// A used approval does not merge a second time: neither a later claim,
	// nor a replay of the same operation, nor two concurrent attempts.
	t.Run("used_approval_merges_once", func(t *testing.T) {
		a := newApprovalAcceptance(t)
		a.approve(3600)
		claim := a.claimOperation()
		if claim == nil || claim.Operation == nil || claim.Operation.ApprovalID != a.approval {
			t.Fatalf("claim = %+v; want the approved merge", claim)
		}
		// Two attempts with the same approval at once.
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = a.process.ExecuteOperation(context.Background(), context.Background(), a.config, *claim)
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		merges, commit := a.merges()
		if len(merges) != 1 || merges[0]["sha"] != a.pushed || commit == "" {
			t.Fatalf("merge requests = %v merge commit %q; want exactly one with sha %s", merges, commit, a.pushed)
		}
		if main := gate2Git(t, a.remote, "rev-parse", "refs/heads/main"); main != commit {
			t.Fatalf("remote main %s; want merge commit %s", main, commit)
		}
		if execution, task, approval, _ := a.state(); execution != "succeeded" || task != "completed" || approval != "consumed" {
			t.Fatalf("state = %s/%s/%s; want succeeded/completed/consumed", execution, task, approval)
		}
		// A later claim offers nothing, and replaying the operation is refused.
		if again := a.claimOperation(); again != nil {
			t.Fatalf("claim after merge = %+v; want none", again)
		}
		if err := a.process.ExecuteOperation(context.Background(), context.Background(), a.config, *claim); err != nil {
			t.Fatal(err)
		}
		// The owner cannot approve the used approval again.
		a.owner(http.MethodPost, "/api/approvals/"+a.approval+"/decision",
			map[string]any{"decision": "approve", "contentVersion": a.pushed}, http.StatusConflict, nil)
		if again := a.claimOperation(); again != nil {
			t.Fatalf("claim after second approval = %+v; want none", again)
		}
		if after, _ := a.merges(); len(after) != 1 {
			t.Fatalf("merge requests after replay = %v; want 1", after)
		}
		if main := gate2Git(t, a.remote, "rev-parse", "refs/heads/main"); main != commit {
			t.Fatalf("remote main moved again to %s", main)
		}
		var uses int
		if err := a.db.QueryRow(`SELECT use_count FROM approvals WHERE id=?`, a.approval).Scan(&uses); err != nil || uses != 1 {
			t.Fatalf("use_count %d %v; want 1", uses, err)
		}
		var used int
		if err := a.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE actor='runner:mac-mini' AND action='approval.consume' AND target_id=?
			AND json_extract(detail,'$.reason')='already_used'`, a.approval).Scan(&used); err != nil || used != 2 {
			t.Fatalf("already_used consume audits %d %v; want 2 (concurrent loser and replay)", used, err)
		}
	})

	// An expired approval does not merge; it waits for re-approval.
	t.Run("expired_approval_does_not_merge", func(t *testing.T) {
		a := newApprovalAcceptance(t)
		a.approve(1)
		var expires string
		if err := a.db.QueryRow(`SELECT expires_at FROM approvals WHERE id=?`, a.approval).Scan(&expires); err != nil {
			t.Fatal(err)
		}
		deadline, err := time.Parse(time.RFC3339Nano, expires)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Until(deadline) + 100*time.Millisecond)
		a.mergeOnce()
		a.assertNotMerged()
		if execution, task, approval, _ := a.state(); execution != "waiting_approval" || task != "waiting_approval" || approval != "expired" {
			t.Fatalf("state = %s/%s/%s; want waiting_approval/waiting_approval/expired", execution, task, approval)
		}
		if claim := a.claimOperation(); claim != nil {
			t.Fatalf("claim after expiry = %+v; want none", claim)
		}
		a.assertNotMerged()
	})
}
