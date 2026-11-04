package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

// Only the unspecified history/copy delivery boundary is bridged in-process.
// Reuse, Git checks, authenticated result reports and DB writes are real.
func newContinueExecution(t *testing.T) (*logControlPlane, *Execution, worktree.Repositories, string) {
	t.Helper()
	f, previous, r, source := newCopyExecution(t)
	gate2Write(t, filepath.Join(source, "dummy config"), "dummy test value\n")
	previous.Claim.Project.CopyAllowlist = []string{"dummy config"}
	if err := previous.CopyWorktreeFiles(context.Background(), r, store.WorktreeCopyStore{DB: f.db}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET status='failed';
		UPDATE worktrees SET status='retained',setup_status='succeeded'`); err != nil {
		t.Fatal(err)
	}
	id := ulid.Make().String()
	if _, err := f.db.Exec(`INSERT INTO executions(id,task_id,seq,mode,status,runner_id,created_at)
		VALUES (?,'task-001',2,'continue','preparing','mac-mini','now')`, id); err != nil {
		t.Fatal(err)
	}
	e := &Execution{Claim: previous.Claim, control: previous.control, status: model.ExecutionPreparing}
	e.Claim.Execution.ID, e.Claim.Execution.Seq = id, 2
	e.Claim.Worktree = &ClaimWorktree{ID: previous.WorktreeID, Path: previous.WorktreePath, BaseSHA: previous.BaseSHA,
		BranchName: previous.BranchName, CopiedFiles: previous.CopiedFiles, SetupStatus: "succeeded"}
	return f, e, r, source
}

func reuseAudit(t *testing.T) WorktreePathAuditor {
	t.Helper()
	return pathAuditFunc(func(context.Context, string) error {
		t.Error("unexpected path rejection")
		return nil
	})
}

func TestContinueWorktreeUncommittedChangesBlockGitStateHTTP(t *testing.T) {
	for _, change := range []string{"unstaged", "staged", "untracked", "changed-copy", "staged-copy", "deleted", "renamed", "corrupt-index"} {
		t.Run(change, func(t *testing.T) {
			f, e, r, _ := newContinueExecution(t)
			w := e.Claim.Worktree
			switch change {
			case "unstaged", "staged":
				gate2Write(t, filepath.Join(w.Path, "source"), "uncommitted")
				if change == "staged" {
					gate2Git(t, w.Path, "add", "source")
				}
			case "untracked":
				gate2Write(t, filepath.Join(w.Path, "other"), "unlisted")
			case "changed-copy":
				gate2Write(t, filepath.Join(w.Path, "dummy config"), "edited")
			case "staged-copy":
				gate2Git(t, w.Path, "add", "dummy config")
			case "deleted":
				if err := os.Remove(filepath.Join(w.Path, "source")); err != nil {
					t.Fatal(err)
				}
			case "renamed":
				gate2Git(t, w.Path, "mv", "source", "renamed")
			case "corrupt-index":
				index := gate2Git(t, w.Path, "rev-parse", "--path-format=absolute", "--git-path", "index")
				gate2Write(t, index, "invalid index")
			}
			head := gate2Git(t, w.Path, "rev-parse", "HEAD")
			err := e.ReuseWorktree(context.Background(), r, store.WorktreeReuseStore{DB: f.db}, reuseAudit(t))
			if !errors.Is(err, worktree.ErrGitState) || e.WorktreePath != "" {
				t.Fatalf("continue should stop: err=%v path=%s", err, e.WorktreePath)
			}
			var execution, task, cause, savedHead, worktreeStatus, id, base, branch string
			if err := f.db.QueryRow(`SELECT e.status,t.status,b.cause,b.head_sha,w.status,e.worktree_id,e.base_sha,e.branch_name
				FROM executions e JOIN tasks t ON t.id=e.task_id JOIN execution_blocks b ON b.execution_id=e.id
				JOIN worktrees w ON w.id=e.worktree_id WHERE e.id=?`, e.Claim.Execution.ID).
				Scan(&execution, &task, &cause, &savedHead, &worktreeStatus, &id, &base, &branch); err != nil ||
				execution != "blocked" || task != "blocked" || cause != "git_state" || savedHead != head || worktreeStatus != "retained" ||
				id != w.ID || base != w.BaseSHA || branch != w.BranchName {
				t.Fatalf("blocked DB snapshot: %s %s %s %s %s %s %s %s err=%v", execution, task, cause, savedHead, worktreeStatus, id, base, branch, err)
			}
			if got := gate2Git(t, w.Path, "rev-parse", "HEAD"); got != head {
				t.Fatal("continue moved branch")
			}
		})
	}
}

func TestContinueWorktreeOnlyUntrackedCopiesAreExemptHTTP(t *testing.T) {
	f, e, r, source := newContinueExecution(t)
	w := e.Claim.Worktree
	before := gate2Git(t, w.Path, "status", "--porcelain")
	if !strings.Contains(before, "?? \"dummy config\"") {
		t.Fatalf("fixture must have an untracked copy: %s", before)
	}
	// Changing the owner's source after copying must not alter the saved
	// exemption or overwrite the task copy during ordinary reuse.
	gate2Write(t, filepath.Join(source, "dummy config"), "new owner content")
	history := store.WorktreeReuseStore{DB: f.db}
	if err := e.ReuseWorktree(context.Background(), r, history, reuseAudit(t)); err != nil {
		t.Fatal(err)
	}
	assertReusedWorktree(t, f, e, "succeeded")
	if gate2Git(t, w.Path, "status", "--porcelain") != before || gate2Git(t, w.Path, "rev-parse", "HEAD") != w.BaseSHA {
		t.Fatal("reuse changed files or HEAD")
	}
	// The measurement ACK may be lost; resend identical metadata without a
	// second state transition, block or row.
	if err := e.control.Reporter.ReportResult(context.Background(), e.Claim.Execution.ID, ExecutionResult{
		FromStatus: model.ExecutionPreparing, Status: model.ExecutionPreparing,
		WorktreeID: w.ID, BaseSHA: w.BaseSHA, BranchName: w.BranchName,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestContinueWorktreeCleanedRowWithRestoredDirectoryRequiresSetupHTTP(t *testing.T) {
	f, e, r, _ := newContinueExecution(t)
	// The directory was already restored before the stage was entered, but
	// activation is still pending (for example after a lost acknowledgement).
	if _, err := f.db.Exec(`UPDATE worktrees SET status='cleaned',cleaned_at='now'`); err != nil {
		t.Fatal(err)
	}
	if err := e.ReuseWorktree(context.Background(), r, store.WorktreeReuseStore{DB: f.db}, reuseAudit(t)); err != nil {
		t.Fatal(err)
	}
	assertReusedWorktree(t, f, e, "pending")
	if e.Claim.Worktree.SetupStatus != "pending" {
		t.Fatal("setup would be skipped using the stale succeeded claim")
	}
}

func assertReusedWorktree(t *testing.T, f *logControlPlane, e *Execution, setup string) {
	t.Helper()
	w := e.Claim.Worktree
	var id, base, branch, execution, status, savedSetup, manifest string
	var cleaned any
	if err := f.db.QueryRow(`SELECT e.worktree_id,e.base_sha,e.branch_name,e.status,w.status,w.setup_status,w.cleaned_at,w.copied_files
		FROM executions e JOIN worktrees w ON w.id=e.worktree_id WHERE e.id=?`, e.Claim.Execution.ID).
		Scan(&id, &base, &branch, &execution, &status, &savedSetup, &cleaned, &manifest); err != nil ||
		id != w.ID || base != w.BaseSHA || branch != w.BranchName || execution != "preparing" || status != "active" || savedSetup != setup || cleaned != nil ||
		e.WorktreeID != id || e.BaseSHA != base || e.BranchName != branch || e.WorktreePath != w.Path {
		t.Fatalf("reuse DB state: %s %s %s %s %s %s cleaned=%v err=%v", id, base, branch, execution, status, savedSetup, cleaned, err)
	}
	var copied []model.CopiedFile
	if err := json.Unmarshal([]byte(manifest), &copied); err != nil || len(copied) != len(e.CopiedFiles) {
		t.Fatalf("manifest=%s memory=%+v err=%v", manifest, e.CopiedFiles, err)
	}
	var blocks, rows int
	if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM execution_blocks),(SELECT count(*) FROM worktrees)`).Scan(&blocks, &rows); err != nil || blocks != 0 || rows != 1 {
		t.Fatalf("blocks=%d worktrees=%d err=%v", blocks, rows, err)
	}
}

func TestContinueWorktreeRecreatesDeletedDirectoryWithoutMovingBranchHTTP(t *testing.T) {
	for _, removal := range []string{"cleaned", "manually-deleted"} {
		t.Run(removal, func(t *testing.T) {
			f, e, r, source := newContinueExecution(t)
			w := e.Claim.Worktree
			// Previous work is ahead of base_sha. Recreating from base instead
			// of from the branch would lose this commit and fail this test.
			gate2Write(t, filepath.Join(w.Path, "source"), "previous harness commit")
			gate2Git(t, w.Path, "add", "source")
			gate2Git(t, w.Path, "commit", "-m", "previous work")
			head := gate2Git(t, w.Path, "rev-parse", "HEAD")
			if head == w.BaseSHA {
				t.Fatal("test needs a branch ahead of base")
			}
			if _, err := f.db.Exec(`UPDATE executions SET harness_head_sha=?,pushed_sha=?,verified_sha=? WHERE id=?`, head, head, head, logExecution); err != nil {
				t.Fatal(err)
			}
			if removal == "cleaned" {
				// Remove just the known dummy copy to let real Git clean up.
				if err := os.Remove(filepath.Join(w.Path, "dummy config")); err != nil {
					t.Fatal(err)
				}
				gate2Git(t, source, "-C", w.Path, "worktree", "remove", w.Path)
				if _, err := f.db.Exec(`UPDATE worktrees SET status='cleaned',cleaned_at='now'`); err != nil {
					t.Fatal(err)
				}
			} else if err := os.RemoveAll(w.Path); err != nil {
				t.Fatal(err)
			}
			// No remote access is available or needed on this route.
			r.Transport = rejectReuseNetwork{t}
			if err := e.ReuseWorktree(context.Background(), r, store.WorktreeReuseStore{DB: f.db}, reuseAudit(t)); err != nil {
				t.Fatal(err)
			}
			assertReusedWorktree(t, f, e, "pending")
			if got := gate2Git(t, w.Path, "rev-parse", "HEAD"); got != head || gate2Git(t, w.Path, "rev-parse", "refs/heads/"+w.BranchName) != head ||
				gate2Git(t, w.Path, "symbolic-ref", "HEAD") != "refs/heads/"+w.BranchName {
				t.Fatalf("restoration moved previous branch: got=%s want=%s", got, head)
			}
			body, err := os.ReadFile(filepath.Join(w.Path, "source"))
			if err != nil || string(body) != "previous harness commit" {
				t.Fatalf("previous committed work lost: %s %v", body, err)
			}
			body, err = os.ReadFile(filepath.Join(w.Path, "dummy config"))
			if err != nil || string(body) != "dummy test value\n" || e.Claim.Worktree.SetupStatus != "pending" {
				t.Fatalf("copy/setup not restored: %s %v", body, err)
			}
		})
	}
}

type rejectReuseNetwork struct{ t *testing.T }

func (r rejectReuseNetwork) Output(context.Context, string, ...string) ([]byte, error) {
	r.t.Error("continue must not fetch or access the remote")
	return nil, errors.New("unexpected network operation")
}

func TestContinueWorktreeChecksKnownHEADAndBranchHTTP(t *testing.T) {
	for _, change := range []string{"moved-head", "harness-head", "pushed-first", "other-branch", "detached", "cleaned-moved-branch"} {
		t.Run(change, func(t *testing.T) {
			f, e, r, _ := newContinueExecution(t)
			w := e.Claim.Worktree
			if change == "other-branch" {
				gate2Git(t, w.Path, "switch", "-c", "unexpected")
			} else if change == "detached" {
				gate2Git(t, w.Path, "checkout", "--detach", "HEAD")
			} else {
				gate2Write(t, filepath.Join(w.Path, "source"), "unexpected commit")
				gate2Git(t, w.Path, "add", "source")
				gate2Git(t, w.Path, "commit", "-m", "additional commit")
			}
			head := gate2Git(t, w.Path, "rev-parse", "HEAD")
			if change == "harness-head" || change == "pushed-first" {
				if _, err := f.db.Exec(`UPDATE executions SET harness_head_sha=? WHERE id=?`, head, logExecution); err != nil {
					t.Fatal(err)
				}
			}
			if change == "pushed-first" {
				if _, err := f.db.Exec(`UPDATE executions SET pushed_sha=?,verified_sha=? WHERE id=?`, w.BaseSHA, w.BaseSHA, logExecution); err != nil {
					t.Fatal(err)
				}
			}
			if change == "cleaned-moved-branch" {
				if err := os.RemoveAll(w.Path); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE worktrees SET status='cleaned',cleaned_at='now'`); err != nil {
					t.Fatal(err)
				}
			}
			err := e.ReuseWorktree(context.Background(), r, store.WorktreeReuseStore{DB: f.db}, reuseAudit(t))
			if change == "harness-head" {
				if err != nil {
					t.Fatal(err)
				}
				assertReusedWorktree(t, f, e, "succeeded")
				return
			}
			if !errors.Is(err, worktree.ErrGitState) {
				t.Fatalf("unexpected Git state was accepted: %v", err)
			}
			var cause, observed, id, base string
			if err := f.db.QueryRow(`SELECT b.cause,b.head_sha,e.worktree_id,e.base_sha FROM execution_blocks b
				JOIN executions e ON e.id=b.execution_id WHERE e.id=?`, e.Claim.Execution.ID).
				Scan(&cause, &observed, &id, &base); err != nil || cause != "git_state" || observed != head || id != w.ID || base != w.BaseSHA {
				t.Fatalf("Git state block=%s %s %s %s err=%v", cause, observed, id, base, err)
			}
			if change == "cleaned-moved-branch" {
				if _, err := os.Lstat(w.Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("mismatched branch must be blocked before restoration: %v", err)
				}
			} else if got := gate2Git(t, w.Path, "rev-parse", "HEAD"); got != head {
				t.Fatal("Git state rejection moved HEAD")
			}
		})
	}
}

func TestContinueWorktreeResultRejectsMetadataChangesHTTP(t *testing.T) {
	for _, change := range []string{"branch", "base", "worktree", "fresh", "other-predecessor"} {
		t.Run(change, func(t *testing.T) {
			f, e, _, _ := newContinueExecution(t)
			w := e.Claim.Worktree
			result := ExecutionResult{FromStatus: model.ExecutionPreparing, Status: model.ExecutionPreparing,
				WorktreeID: w.ID, BaseSHA: w.BaseSHA, BranchName: w.BranchName}
			switch change {
			case "branch":
				result.BranchName += "-different"
			case "base":
				result.BaseSHA = strings.Repeat("a", 40)
			case "worktree":
				result.WorktreeID = ulid.Make().String()
			case "fresh":
				if _, err := f.db.Exec(`UPDATE executions SET mode='fresh' WHERE id=?`, e.Claim.Execution.ID); err != nil {
					t.Fatal(err)
				}
			case "other-predecessor":
				if _, err := f.db.Exec(`UPDATE executions SET worktree_id=NULL WHERE id=?`, logExecution); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.control.Reporter.ReportResult(context.Background(), e.Claim.Execution.ID, result); err == nil || !strings.Contains(err.Error(), "HTTP 422") {
				t.Fatalf("invalid metadata report err=%v", err)
			}
			var untouched bool
			if err := f.db.QueryRow(`SELECT worktree_id IS NULL AND base_sha IS NULL AND branch_name IS NULL
				AND status='preparing' AND (SELECT status FROM worktrees WHERE id=?)='retained'
				FROM executions WHERE id=?`, w.ID, e.Claim.Execution.ID).Scan(&untouched); err != nil || !untouched {
				t.Fatalf("rejected report changed DB: %v %v", untouched, err)
			}
		})
	}
}

func TestContinueWorktreePathRejectionAuditedAndPreservesOwnerHTTP(t *testing.T) {
	f, e, r, owner := newContinueExecution(t)
	w := e.Claim.Worktree
	before := baseOwnerSnapshot(t, owner)
	// Substitute the linked worktree pointer with the owner's repository.
	gate2Write(t, filepath.Join(w.Path, ".git"), "gitdir: "+filepath.Join(owner, ".git")+"\n")
	audits := 0
	err := e.ReuseWorktree(context.Background(), r, store.WorktreeReuseStore{DB: f.db}, pathAuditFunc(func(context.Context, string) error {
		audits++
		return nil
	}))
	if err == nil || audits != 1 {
		t.Fatalf("unsafe .git err=%v audits=%d", err, audits)
	}
	var status, class string
	if err := f.db.QueryRow(`SELECT status,error_class FROM executions WHERE id=?`, e.Claim.Execution.ID).Scan(&status, &class); err != nil || status != "failed" || class != "config_error" {
		t.Fatalf("path failure=%s %s %v", status, class, err)
	}
	after := baseOwnerSnapshot(t, owner)
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) != string(afterJSON) {
		t.Fatal("owner Git area or files changed")
	}
}

func TestContinueWorktreeRequiresHistoryAndResultAcknowledgement(t *testing.T) {
	for _, failure := range []string{"history", "ack", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			f, e, r, _ := newContinueExecution(t)
			if failure == "history" {
				if _, err := f.db.Exec(`UPDATE executions SET base_sha=NULL WHERE id=?`, logExecution); err != nil {
					t.Fatal(err)
				}
			}
			e.control.Reporter = reportFunc(func(context.Context, string, ExecutionResult) error { return errors.New("no ACK") })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "cancelled" {
				cancel()
			}
			err := e.ReuseWorktree(ctx, r, store.WorktreeReuseStore{DB: f.db}, reuseAudit(t))
			if err == nil || e.WorktreePath != "" || e.WorktreeID != "" || e.BaseSHA != "" ||
				failure == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("reuse proceeded without state/ACK: err=%v id=%s path=%s base=%s", err, e.WorktreeID, e.WorktreePath, e.BaseSHA)
			}
		})
	}
}
