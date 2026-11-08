package runner

import (
	"database/sql"
	"strings"
	"testing"
)

// Spec 17 3.7 through the production pipeline, the real Control Plane and
// SQLite. The remote is a local bare repository served over Git HTTP; GitHub
// REST is a test server that counts PR searches and creations.

// direct_push: the verified commit is pushed to the Base Branch, the remote
// Base Branch SHA equals verified_sha and no PR request reaches GitHub.
func TestProductionPipelineDirectPushToBaseBranchWithoutPR(t *testing.T) {
	f := newProductionPipelineFixture(t, "direct_push")
	if err := f.execute(t); err != nil {
		t.Fatal(err)
	}
	f.assertSucceeded(t)
	var verified string
	if err := f.db.QueryRow(`SELECT verified_sha FROM executions WHERE id=?`, logExecution).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if verified == f.initialMain {
		t.Fatal("fixture did not produce a new commit")
	}
}

// 3.7.1 step 2: the owner switches to require_approval right before the
// push. The Runner follows the current policy: it pushes the task branch,
// creates a PR, the Base Branch stays put and the task waits for approval.
func TestProductionPipelineDirectPushPolicyChangedTakesPRPath(t *testing.T) {
	f := newProductionPipelineFixture(t, "direct_push_policy_changed")
	if err := f.execute(t); err != nil {
		t.Fatal(err)
	}
	if f.publishChecks != 1 {
		t.Fatalf("publish checks %d", f.publishChecks)
	}
	// require_approval now: assertSucceeded expects waiting_approval, one
	// PR, the task branch at verified_sha and main unchanged.
	f.assertSucceeded(t)
}

// 3.7.1 (Fail Closed): when publish-check cannot reach the Control Plane,
// nothing is pushed and no PR is created.
func TestProductionPipelineDirectPushUnreachableControlPlaneDoesNotPush(t *testing.T) {
	f := newProductionPipelineFixture(t, "direct_push_unreachable")
	if err := f.execute(t); err == nil {
		t.Fatal("expected publish failure")
	}
	var status, class string
	var pushed, pr *string
	if err := f.db.QueryRow(`SELECT e.status,coalesce(e.error_class,''),e.pushed_sha,e.pr_url FROM executions e WHERE e.id=?`, logExecution).
		Scan(&status, &class, &pushed, &pr); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || class != "publish_failed" || pushed != nil || pr != nil {
		t.Fatalf("execution %s/%s pushed=%v pr=%v", status, class, pushed, pr)
	}
	if f.publishChecks != 1 || f.prCalls != 0 || f.prSearches != 0 {
		t.Fatalf("checks/PR/search %d/%d/%d", f.publishChecks, f.prCalls, f.prSearches)
	}
	if got := gate2Git(t, f.remote, "rev-parse", "refs/heads/main"); got != f.initialMain {
		t.Fatalf("remote main moved to %s", got)
	}
	if refs := gate2Git(t, f.remote, "for-each-ref", "--format=%(refname)", "refs/heads/"); refs != "refs/heads/main" {
		t.Fatalf("remote refs changed: %s", refs)
	}
}

// 3.7.1 step 1: a cancellation request returned by publish-check stops the
// Execution before the push.
func TestProductionPipelineDirectPushCancellationDoesNotPush(t *testing.T) {
	f := newProductionPipelineFixture(t, "direct_push_cancelled")
	if err := f.execute(t); err == nil {
		t.Fatal("expected cancellation")
	}
	var status string
	var pushed *string
	if err := f.db.QueryRow(`SELECT status,pushed_sha FROM executions WHERE id=?`, logExecution).Scan(&status, &pushed); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || pushed != nil || f.publishChecks != 1 || f.prCalls != 0 {
		t.Fatalf("execution %s pushed=%v checks=%d PR=%d", status, pushed, f.publishChecks, f.prCalls)
	}
	if refs := gate2Git(t, f.remote, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/"); refs != f.initialMain+" refs/heads/main" {
		t.Fatalf("remote refs changed: %s", refs)
	}
}

// 3.7.3: the remote Base Branch moved past the task's starting point before
// the direct push. The Runner does not force the push; it takes the PR path.
// The Base Branch keeps the other commit, the task branch carries
// verified_sha and exactly one PR is created against the Base Branch. The
// Project is still direct_push, so no approval is opened (Evaloom does not
// merge this PR; the owner merges it on GitHub) and the task completes with
// the PR recorded, as with Phase 1 publication.
func TestProductionPipelineDirectPushNotFastForwardTakesPRPathWithoutForce(t *testing.T) {
	f := newProductionPipelineFixture(t, "direct_push_not_fast_forward")
	if err := f.execute(t); err != nil {
		t.Fatal(err)
	}
	if f.advancedMain == "" || f.advancedMain == f.initialMain {
		t.Fatal("fixture did not advance the remote Base Branch")
	}
	if got := gate2Git(t, f.remote, "rev-parse", "refs/heads/main"); got != f.advancedMain {
		t.Fatalf("remote main %s, want the advanced %s (forced push?)", got, f.advancedMain)
	}
	var status, taskStatus, branch, base, verified, pushed string
	var errorClass sql.NullString
	var pr sql.NullInt64
	var prURL sql.NullString
	if err := f.db.QueryRow(`SELECT e.status,t.status,e.branch_name,e.base_sha,e.verified_sha,e.pushed_sha,e.pr_number,e.pr_url,e.error_class
		FROM executions e JOIN tasks t ON t.id=e.task_id WHERE e.id=?`, logExecution).
		Scan(&status, &taskStatus, &branch, &base, &verified, &pushed, &pr, &prURL, &errorClass); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || taskStatus != "completed" || errorClass.Valid {
		t.Fatalf("execution %s task %s class %v", status, taskStatus, errorClass)
	}
	if base != f.initialMain || verified == "" || verified == f.initialMain || pushed != verified {
		t.Fatalf("base %s verified %s pushed %s", base, verified, pushed)
	}
	if !pr.Valid || pr.Int64 != 1 || prURL.String != "https://github.com/owner/repo/pull/1" {
		t.Fatalf("PR %v %v", pr, prURL)
	}
	if got := gate2Git(t, f.remote, "rev-parse", "refs/heads/"+branch); got != verified {
		t.Fatalf("task branch %s != verified %s", got, verified)
	}
	if f.publishChecks != 1 || f.prSearches != 1 || f.prCalls != 1 || !strings.Contains(f.prBodies[0], "Gate 3: pass") {
		t.Fatalf("checks/search/PR %d/%d/%d", f.publishChecks, f.prSearches, f.prCalls)
	}
	var approvals int
	if err := f.db.QueryRow(`SELECT count(*) FROM approvals WHERE execution_id=?`, logExecution).Scan(&approvals); err != nil || approvals != 0 {
		t.Fatalf("approvals %d %v", approvals, err)
	}
	for kind, want := range map[string]int{"pushed": 1, "pr_created": 1} {
		var count int
		if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE execution_id=? AND type=?`, logExecution, kind).Scan(&count); err != nil || count != want {
			t.Fatalf("event %s count %d %v", kind, count, err)
		}
	}
	var mismatches int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action LIKE '%mismatch%'`).Scan(&mismatches); err != nil || mismatches != 0 {
		t.Fatalf("remote mismatch audits %d %v", mismatches, err)
	}
}
