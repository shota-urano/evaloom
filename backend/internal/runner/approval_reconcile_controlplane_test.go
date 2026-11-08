package runner

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"
)

// waitForApproval puts the fixture back to a PR waiting for the owner's
// approval (spec 17 3.3): a pending approval and a waiting_approval Execution.
func (f *operationFixture) waitForApproval() {
	f.t.Helper()
	if _, err := f.db.Exec(`UPDATE approvals SET status='pending', decided_by=NULL, decided_at=NULL, expires_at=NULL WHERE id=?`, f.approvalID); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET status='waiting_approval' WHERE id=?`, operationExecution); err != nil {
		f.t.Fatal(err)
	}
}

func (f *operationFixture) reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prReads
}

// checkOnce drives one periodic check like Run does: a heartbeat to learn the
// waiting PRs, then one pass over them.
func (f *operationFixture) checkOnce() error {
	f.t.Helper()
	if err := f.process.Heartbeat(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	return f.process.CheckWaitingApprovals(context.Background())
}

func (f *operationFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// Acceptance (spec 17 9章, 3.6, Q58): the owner merges or closes the PR on
// GitHub while it waits for approval. One periodic check reads the PR from
// the test GitHub API and the Control Plane invalidates the approval
// (target_changed); a merge completes the task, a close fails it. No merge
// request is sent by Evaloom.
func TestPeriodicCheckOfPullRequestWaitingForApproval(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		merged                    bool
		execution, task, message  string
		eventReason, notification string
	}{
		{"merged on GitHub", true, "succeeded", "completed", "", "pull_request_merged", "succeeded"},
		{"closed on GitHub", false, "failed", "failed", "pull_request_closed", "pull_request_closed", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOperationFixture(t)
			f.waitForApproval()
			f.process.EnableApprovalCheck(f.config)
			f.mu.Lock()
			f.prOpen, f.prMerged = false, tc.merged
			f.mu.Unlock()
			if err := f.checkOnce(); err != nil {
				t.Fatal(err)
			}
			if reads := f.reads(); reads != 1 {
				t.Fatalf("PR reads = %d; want 1", reads)
			}
			if merges := f.mergeRequests(); len(merges) != 0 {
				t.Fatalf("merge requests = %v; want none", merges)
			}
			if execution, task, approval, reason := f.state(); execution != tc.execution || task != tc.task ||
				approval != "invalidated" || reason != "target_changed" {
				t.Fatalf("state = %s/%s/%s/%s; want %s/%s/invalidated/target_changed", execution, task, approval, reason, tc.execution, tc.task)
			}
			var class, message sql.NullString
			var uses int
			if err := f.db.QueryRow(`SELECT e.error_class, e.error_message, a.use_count FROM executions e JOIN approvals a ON a.execution_id=e.id WHERE a.id=?`,
				f.approvalID).Scan(&class, &message, &uses); err != nil {
				t.Fatal(err)
			}
			if class.Valid || message.String != tc.message || uses != 0 {
				t.Fatalf("recorded %v/%q uses %d; want no error class, message %q, unused approval", class, message.String, uses, tc.message)
			}
			if n := f.count(`SELECT count(*) FROM audit_logs WHERE actor='system' AND action='approval.invalidate' AND target_id=?
				AND json_extract(detail,'$.reason')='target_changed'`, f.approvalID); n != 1 {
				t.Fatalf("invalidation audits = %d; want 1", n)
			}
			audit := `SELECT count(*) FROM audit_logs WHERE actor='runner:mac-mini' AND action='execution.result' AND target_id=?
				AND json_extract(detail,'$.from')='waiting_approval' AND json_extract(detail,'$.to')=? AND json_extract(detail,'$.reason')=?`
			args := []any{operationExecution, tc.execution, tc.eventReason}
			if tc.merged {
				audit += ` AND json_extract(detail,'$.mergeCommitSha')=?`
				args = append(args, operationMergeCommit)
			}
			if n := f.count(audit, args...); n != 1 {
				t.Fatalf("result audits = %d; want 1", n)
			}
			if n := f.count(`SELECT count(*) FROM execution_events WHERE execution_id=? AND type='status_changed'
				AND json_extract(payload,'$.from')='waiting_approval' AND json_extract(payload,'$.reason')=?`, operationExecution, tc.eventReason); n != 1 {
				t.Fatalf("status events = %d; want 1", n)
			}
			if n := f.count(`SELECT count(*) FROM notification_requests WHERE execution_id=? AND status=?`, operationExecution, tc.notification); n != 1 {
				t.Fatalf("notifications = %d; want 1", n)
			}
			// The finished task is no longer waiting: the next check reads nothing.
			if err := f.checkOnce(); err != nil {
				t.Fatal(err)
			}
			if reads := f.reads(); reads != 1 {
				t.Fatalf("PR reads after finish = %d; want 1", reads)
			}
		})
	}
}

// An open PR is left waiting for approval; an approval being merged by
// Evaloom (merging) is not read by the periodic check.
func TestPeriodicCheckLeavesOpenPullRequestWaiting(t *testing.T) {
	f := newOperationFixture(t)
	f.waitForApproval()
	f.process.EnableApprovalCheck(f.config)
	if err := f.checkOnce(); err != nil {
		t.Fatal(err)
	}
	if reads := f.reads(); reads != 1 {
		t.Fatalf("PR reads = %d; want 1", reads)
	}
	if execution, task, approval, reason := f.state(); execution != "waiting_approval" || task != "waiting_approval" || approval != "pending" || reason != "" {
		t.Fatalf("state = %s/%s/%s/%s; want unchanged", execution, task, approval, reason)
	}
	f.approve(time.Now().Add(time.Hour))
	if err := f.checkOnce(); err != nil {
		t.Fatal(err)
	}
	if reads := f.reads(); reads != 1 {
		t.Fatalf("PR reads while merging = %d; want 1", reads)
	}
}

// fakeClock is the injected clock of the periodic check.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Run checks the waiting PRs at most once per interval of the injected
// clock, however often it heartbeats.
func TestRunChecksWaitingPullRequestsOncePerInterval(t *testing.T) {
	f := newOperationFixture(t)
	f.waitForApproval()
	f.process.EnableApprovalCheck(f.config)
	reader := f.process.approvals.reader
	clock := &fakeClock{now: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
	f.process.setApprovalCheck(reader, approvalCheckInterval, clock.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- f.process.Run(ctx, func(context.Context, Claim) { t.Error("unexpected claim") }, func(err error) { t.Error(err) })
	}()
	waitFor := func(want int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for f.reads() < want {
			if time.Now().After(deadline) {
				t.Fatalf("PR reads = %d; want %d", f.reads(), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor(1)
	// Many heartbeats (poll interval 20ms) pass without the clock moving.
	time.Sleep(200 * time.Millisecond)
	if reads := f.reads(); reads != 1 {
		t.Fatalf("PR reads before the interval = %d; want 1", reads)
	}
	clock.advance(approvalCheckInterval)
	waitFor(2)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if reads := f.reads(); reads != 2 {
		t.Fatalf("PR reads = %d; want 2", reads)
	}
}
