package runner

import (
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
