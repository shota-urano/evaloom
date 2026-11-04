package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

type cleanupPRFunc func(context.Context, string, int) (publisher.PullRequestState, error)

func (f cleanupPRFunc) State(ctx context.Context, url string, number int) (publisher.PullRequestState, error) {
	return f(ctx, url, number)
}

func closedCleanupPR(t *testing.T, url string, state publisher.PullRequestState) cleanupPRFunc {
	t.Helper()
	return func(_ context.Context, got string, number int) (publisher.PullRequestState, error) {
		if got != url || number != 12 {
			t.Fatalf("cleanup PR lookup = %s #%d", got, number)
		}
		return state, nil
	}
}

// Real Git directories, authenticated result reports and both production
// recovery paths verify retention. No production filesystem removal is mocked.
func TestWorktreeUnsuccessfulStopsRetainDirectoryBranchAndStatus(t *testing.T) {
	for _, stop := range []string{"failed", "timed_out", "cancelled", "blocked", "boot-changed", "heartbeat-lost"} {
		t.Run(stop, func(t *testing.T) {
			f, e, r, _ := newCopyExecution(t)
			ctx := context.Background()
			if err := e.CopyWorktreeFiles(ctx, r, store.WorktreeCopyStore{DB: f.db}); err != nil {
				t.Fatal(err)
			}
			gate2Write(t, filepath.Join(e.WorktreePath, "uncommitted"), "retain this work")
			switch stop {
			case "boot-changed":
				if _, err := store.Heartbeat(ctx, f.db, f.config.RunnerID, "before", nil); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Heartbeat(ctx, f.db, f.config.RunnerID, "after", nil); err != nil {
					t.Fatal(err)
				}
			case "heartbeat-lost":
				start := time.Now().UTC()
				if _, err := f.db.Exec(`UPDATE runners SET last_heartbeat_at=?`, start.Add(-time.Hour).Format(time.RFC3339Nano)); err != nil {
					t.Fatal(err)
				}
				if err := store.RecoverStaleRunners(ctx, f.db, start, start.Add(store.StartupHeartbeatGrace)); err != nil {
					t.Fatal(err)
				}
			default:
				var block *ResultBlock
				var class model.ErrorClass
				if stop == "failed" {
					class = model.ErrorConfig
				}
				if stop == "blocked" {
					block = &ResultBlock{Cause: model.BlockGitState, HeadSHA: e.BaseSHA}
				}
				if err := e.reportResult(model.ExecutionStatus(stop), class, block, ""); err != nil {
					t.Fatal(err)
				}
			}
			var execution, status string
			var cleaned any
			if err := f.db.QueryRow(`SELECT e.status,w.status,w.cleaned_at FROM executions e JOIN worktrees w ON w.id=e.worktree_id WHERE e.id=?`,
				e.Claim.Execution.ID).Scan(&execution, &status, &cleaned); err != nil || status != "retained" || cleaned != nil {
				t.Fatalf("retained snapshot: execution=%s worktree=%s cleaned=%v err=%v", execution, status, cleaned, err)
			}
			if strings.Contains(stop, "-") && execution != "interrupted" || !strings.Contains(stop, "-") && execution != stop {
				t.Fatalf("execution status = %s for %s", execution, stop)
			}
			if body, err := os.ReadFile(filepath.Join(e.WorktreePath, "uncommitted")); err != nil || string(body) != "retain this work" {
				t.Fatalf("uncommitted work lost: %q %v", body, err)
			}
			if gate2Git(t, e.WorktreePath, "rev-parse", "refs/heads/"+e.BranchName) != e.BaseSHA {
				t.Fatal("retention moved branch")
			}
		})
	}
}

func newCleanupExecution(t *testing.T) (*logControlPlane, *Execution, worktree.Repositories, string) {
	t.Helper()
	f, e, r, source := newCopyExecution(t)
	if err := e.CopyWorktreeFiles(context.Background(), r, store.WorktreeCopyStore{DB: f.db}); err != nil {
		t.Fatal(err)
	}
	// Preserve real work ahead of the original base, not just an empty branch.
	gate2Write(t, filepath.Join(e.WorktreePath, "source"), "committed work")
	gate2Git(t, e.WorktreePath, "add", "source")
	gate2Git(t, e.WorktreePath, "commit", "-m", "work to preserve")
	head := gate2Git(t, e.WorktreePath, "rev-parse", "HEAD")
	if err := e.reportResult(model.ExecutionFailed, model.ErrorConfig, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET pr_number=12,harness_head_sha=? WHERE id=?`, head, e.Claim.Execution.ID); err != nil {
		t.Fatal(err)
	}
	return f, e, r, source
}

func TestCleanupWorktreeKeepsBranchAndContinueRestoresIt(t *testing.T) {
	for _, state := range []publisher.PullRequestState{publisher.PRClosed, publisher.PRMerged} {
		t.Run(string(state), func(t *testing.T) {
			f, previous, r, _ := newCleanupExecution(t)
			ctx := context.Background()
			head := gate2Git(t, previous.WorktreePath, "rev-parse", "HEAD")
			if err := CleanupWorktree(ctx, f.config.RunnerID, previous.WorktreeID, store.WorktreeCleanupStore{DB: f.db}, r,
				closedCleanupPR(t, previous.Claim.Project.GitRemoteURL, state), reuseAudit(t)); err != nil {
				t.Fatal(err)
			}
			var status, setup, manifest, cleaned string
			if err := f.db.QueryRow(`SELECT status,setup_status,copied_files,cleaned_at FROM worktrees WHERE id=?`, previous.WorktreeID).
				Scan(&status, &setup, &manifest, &cleaned); err != nil || status != "cleaned" || cleaned == "" || setup != "pending" || manifest != "[]" {
				t.Fatalf("cleaned row: %s %s %s %s err=%v", status, setup, manifest, cleaned, err)
			}
			if _, err := os.Lstat(previous.WorktreePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cleanup left directory: %v", err)
			}
			repository := filepath.Join(r.WorktreeRoot, ".evaloom-repositories", previous.Claim.Project.ID)
			if got := gate2Git(t, repository, "rev-parse", "--verify", "refs/heads/"+previous.BranchName); got != head {
				t.Fatalf("cleanup branch = %s want %s", got, head)
			}
			// Identical requests do not remove anything twice or change the time.
			if err := CleanupWorktree(ctx, f.config.RunnerID, previous.WorktreeID, store.WorktreeCleanupStore{DB: f.db}, r,
				cleanupPRFunc(func(context.Context, string, int) (publisher.PullRequestState, error) {
					t.Fatal("already cleaned worktree queried GitHub again")
					return "", nil
				}), reuseAudit(t)); err != nil {
				t.Fatal(err)
			}
			var unchanged bool
			if err := f.db.QueryRow(`SELECT cleaned_at=? FROM worktrees WHERE id=?`, cleaned, previous.WorktreeID).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("cleanup timestamp changed: %v", err)
			}
			// The existing reuse implementation must recreate from that branch,
			// preserving the recorded base and the additional commit.
			const next = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
			if _, err := f.db.Exec(`INSERT INTO executions(id,task_id,seq,mode,status,runner_id,created_at)
				VALUES (?,'task-001',2,'continue','preparing','mac-mini','now'); UPDATE tasks SET status='running'`, next); err != nil {
				t.Fatal(err)
			}
			e := &Execution{Claim: previous.Claim, control: previous.control, status: model.ExecutionPreparing}
			e.Claim.Execution.ID, e.Claim.Execution.Seq = next, 2
			e.Claim.Worktree = &ClaimWorktree{ID: previous.WorktreeID, Path: previous.WorktreePath, BaseSHA: previous.BaseSHA,
				BranchName: previous.BranchName, CopiedFiles: previous.CopiedFiles, SetupStatus: setup}
			if err := e.ReuseWorktree(ctx, r, store.WorktreeReuseStore{DB: f.db}, reuseAudit(t)); err != nil {
				t.Fatal(err)
			}
			assertReusedWorktree(t, f, e, "pending")
			if gate2Git(t, e.WorktreePath, "rev-parse", "HEAD") != head || e.BaseSHA != previous.BaseSHA {
				t.Fatal("cleanup/continue lost branch or base SHA")
			}
		})
	}
}

func TestCleanupWorktreeRejectsIneligibleOrUnsafeTargets(t *testing.T) {
	for _, problem := range []string{"open-pr", "unknown-pr", "pr-error", "no-pr", "wrong-runner", "queued", "running", "blocked", "dirty", "locked", "wrong-branch", "symlink"} {
		t.Run(problem, func(t *testing.T) {
			f, e, r, source := newCleanupExecution(t)
			ctx := context.Background()
			head := gate2Git(t, e.WorktreePath, "rev-parse", "HEAD")
			runnerID, state := f.config.RunnerID, publisher.PRClosed
			calls, audits := 0, 0
			switch problem {
			case "open-pr":
				state = publisher.PROpen
			case "unknown-pr":
				state = "unknown"
			case "wrong-runner":
				runnerID = "other"
			case "no-pr":
				if _, err := f.db.Exec(`UPDATE executions SET pr_number=NULL`); err != nil {
					t.Fatal(err)
				}
			case "queued", "running", "blocked":
				status := map[string]string{"queued": "pending", "running": "running", "blocked": "blocked"}[problem]
				if _, err := f.db.Exec(`UPDATE tasks SET status=?`, problem); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE executions SET status=?`, status); err != nil {
					t.Fatal(err)
				}
			case "dirty":
				gate2Write(t, filepath.Join(e.WorktreePath, "keep"), "untracked")
			case "locked":
				gate2Git(t, e.WorktreePath, "worktree", "lock", e.WorktreePath)
			case "wrong-branch":
				gate2Git(t, e.WorktreePath, "switch", "-c", "other")
			case "symlink":
				// Retain the actual checkout but replace its path with an owner link.
				if err := os.Rename(e.WorktreePath, e.WorktreePath+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(source, e.WorktreePath); err != nil {
					t.Fatal(err)
				}
			}
			before := baseOwnerSnapshot(t, source)
			err := CleanupWorktree(ctx, runnerID, e.WorktreeID, store.WorktreeCleanupStore{DB: f.db}, r,
				cleanupPRFunc(func(context.Context, string, int) (publisher.PullRequestState, error) {
					calls++
					if problem == "pr-error" {
						return "", errors.New("credential=secret")
					}
					return state, nil
				}), pathAuditFunc(func(context.Context, string) error { audits++; return nil }))
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("ineligible cleanup err=%v", err)
			}
			if problem == "symlink" && audits != 1 || problem != "symlink" && audits != 0 {
				t.Fatalf("path rejection audits = %d", audits)
			}
			if (problem == "no-pr" || problem == "wrong-runner" || problem == "queued" || problem == "running" || problem == "blocked") && calls != 0 {
				t.Fatal("ineligible task looked up PR")
			}
			var unchanged bool
			if err := f.db.QueryRow(`SELECT status='retained' AND cleaned_at IS NULL FROM worktrees`).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("rejection changed worktree status: %v %v", unchanged, err)
			}
			if _, err := os.Lstat(e.WorktreePath); err != nil {
				t.Fatalf("rejected cleanup removed directory: %v", err)
			}
			repository := filepath.Join(r.WorktreeRoot, ".evaloom-repositories", e.Claim.Project.ID)
			if gate2Git(t, repository, "rev-parse", "refs/heads/"+e.BranchName) != head {
				t.Fatal("rejected cleanup changed branch")
			}
			after := baseOwnerSnapshot(t, source)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("cleanup changed owner checkout")
			}
		})
	}
}

func TestCleanupWorktreeDBFailureCanRetryAfterDirectoryRemoval(t *testing.T) {
	f, e, r, _ := newCleanupExecution(t)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_cleanup BEFORE UPDATE OF status ON worktrees
		WHEN NEW.status='cleaned' BEGIN SELECT RAISE(ABORT, 'reject cleanup'); END`); err != nil {
		t.Fatal(err)
	}
	clean := func() error {
		return CleanupWorktree(context.Background(), f.config.RunnerID, e.WorktreeID, store.WorktreeCleanupStore{DB: f.db}, r,
			closedCleanupPR(t, e.Claim.Project.GitRemoteURL, publisher.PRClosed), reuseAudit(t))
	}
	if err := clean(); err == nil {
		t.Fatal("cleanup DB failure reported success")
	}
	var unchanged bool
	if err := f.db.QueryRow(`SELECT status='retained' AND cleaned_at IS NULL FROM worktrees`).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("DB failure partially recorded cleanup: %v", err)
	}
	if _, err := os.Lstat(e.WorktreePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test must remove directory before DB failure: %v", err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER reject_cleanup`); err != nil {
		t.Fatal(err)
	}
	if err := clean(); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupWorktreeExcludesConcurrentLifecycleWrites(t *testing.T) {
	f, e, r, _ := newCleanupExecution(t)
	other, err := store.Open(context.Background(), f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := CleanupWorktree(context.Background(), f.config.RunnerID, e.WorktreeID, store.WorktreeCleanupStore{DB: f.db}, r,
		cleanupPRFunc(func(ctx context.Context, _ string, _ int) (publisher.PullRequestState, error) {
			// A second CP connection cannot queue a retry while removal is in
			// progress. BEGIN IMMEDIATE, not just the in-process pool, excludes it.
			if _, err := other.ExecContext(ctx, `UPDATE tasks SET status='queued'`); err == nil {
				t.Fatal("lifecycle write raced cleanup")
			}
			return publisher.PRClosed, nil
		}), reuseAudit(t)); err != nil {
		t.Fatal(err)
	}
}

func TestSucceededWorktreeStaysActiveAndOpenPRCannotBeCleaned(t *testing.T) {
	f, e, r, _ := newCopyExecution(t)
	ctx := context.Background()
	if err := e.CopyWorktreeFiles(ctx, r, store.WorktreeCopyStore{DB: f.db}); err != nil {
		t.Fatal(err)
	}
	from := model.ExecutionPreparing
	for _, to := range []model.ExecutionStatus{model.ExecutionRunning, model.ExecutionVerifying, model.ExecutionPublishing, model.ExecutionSucceeded} {
		fromTask, err := task.TaskStatusForExecution(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := task.TransitionExecution(ctx, f.db, task.Transition{TaskID: e.Claim.Execution.TaskID, ExecutionID: e.Claim.Execution.ID,
			FromTask: fromTask, FromExecution: from, ToExecution: to}); err != nil {
			t.Fatal(err)
		}
		from = to
	}
	if _, err := f.db.Exec(`UPDATE executions SET pr_number=12`); err != nil {
		t.Fatal(err)
	}
	if err := CleanupWorktree(ctx, f.config.RunnerID, e.WorktreeID, store.WorktreeCleanupStore{DB: f.db}, r,
		closedCleanupPR(t, e.Claim.Project.GitRemoteURL, publisher.PROpen), reuseAudit(t)); err == nil {
		t.Fatal("completed task with open PR was cleaned")
	}
	var active bool
	if err := f.db.QueryRow(`SELECT status='active' AND cleaned_at IS NULL FROM worktrees`).Scan(&active); err != nil || !active {
		t.Fatalf("successful worktree must stay active: %v", err)
	}
	if _, err := os.Stat(e.WorktreePath); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupCancellationKeepsReservationUntilRemovalReturns(t *testing.T) {
	f, e, _, _ := newCleanupExecution(t)
	other, err := store.Open(context.Background(), f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = (store.WorktreeCleanupStore{DB: f.db}).WithWorktreeCleanup(ctx, f.config.RunnerID, e.WorktreeID,
		func(context.Context, model.WorktreeCleanupTarget) error {
			cancel()
			// Give database/sql time to react to cancellation, while a Runner
			// callback is still winding down. It must retain the transaction.
			probe, done := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer done()
			if err := f.db.QueryRowContext(probe, `SELECT 1`).Scan(new(int)); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cleanup released its connection during removal: %v", err)
			}
			if _, err := other.Exec(`UPDATE tasks SET status='queued'`); err == nil {
				t.Fatal("cancelled cleanup released lifecycle exclusion too early")
			}
			return nil
		})
	if err == nil {
		t.Fatal("cancelled cleanup recorded success")
	}
	var retained bool
	if err := f.db.QueryRow(`SELECT status='retained' AND cleaned_at IS NULL FROM worktrees`).Scan(&retained); err != nil || !retained {
		t.Fatalf("cancelled cleanup changed status: %v", err)
	}
}
