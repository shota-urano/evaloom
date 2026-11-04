package runner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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

func TestContinueWorktreeCleanedRowWithExistingDirectoryBlocksHTTP(t *testing.T) {
	f, e, r, _ := newContinueExecution(t)
	if _, err := f.db.Exec(`UPDATE worktrees SET status='cleaned',cleaned_at='now'`); err != nil {
		t.Fatal(err)
	}
	if err := e.ReuseWorktree(context.Background(), r, store.WorktreeReuseStore{DB: f.db}, reuseAudit(t)); !errors.Is(err, worktree.ErrGitState) {
		t.Fatalf("inconsistent cleaned row did not block: %v", err)
	}
	var untouched bool
	if err := f.db.QueryRow(`SELECT status='cleaned' AND cleaned_at='now' AND setup_status='succeeded' FROM worktrees`).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("inconsistent worktree was changed: %v %v", untouched, err)
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
	for _, file := range copied {
		body, err := os.ReadFile(filepath.Join(w.Path, file.Path))
		if err != nil || file.SHA256 != fmt.Sprintf("%x", sha256.Sum256(body)) {
			t.Fatalf("saved copy hash disagrees with file: %+v %v", file, err)
		}
	}
	var blocks, rows int
	if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM execution_blocks WHERE execution_id=?),(SELECT count(*) FROM worktrees)`, e.Claim.Execution.ID).Scan(&blocks, &rows); err != nil || blocks != 0 || rows != 1 {
		t.Fatalf("blocks=%d worktrees=%d err=%v", blocks, rows, err)
	}
}

func TestContinueWorktreeRecreatesDeletedDirectoryWithoutMovingBranchHTTP(t *testing.T) {
	for _, removal := range []string{"cleaned", "manually-deleted"} {
		t.Run(removal, func(t *testing.T) {
			f, e, r, source := newContinueExecution(t)
			w := e.Claim.Worktree
			const newCopy = "new dummy copy after cleanup\n"
			gate2Write(t, filepath.Join(source, "dummy config"), newCopy)
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
			history := store.WorktreeReuseStore{DB: f.db}
			// Lost/duplicated deliveries cannot clear saved copies or reset
			// an already acknowledged generation.
			if err := history.BeginWorktreeRestore(context.Background(), e.Claim.Execution.ID, w.ID); err != nil {
				t.Fatal(err)
			}
			prepared := model.PreparedWorktree{Path: w.Path, BranchName: w.BranchName, BaseSHA: w.BaseSHA, CopiedFiles: e.CopiedFiles}
			if err := history.RecordRestoredCopies(context.Background(), e.Claim.Execution.ID, w.ID, prepared); err != nil {
				t.Fatal(err)
			}
			assertReusedWorktree(t, f, e, "pending")
			if succeeded, err := (remoteSetupRepository{execution: e}).SetupSucceeded(context.Background(), e.Claim.Execution.ID, w.ID); err != nil || succeeded {
				t.Fatalf("restored setup would be skipped: %v %v", succeeded, err)
			}
			if got := gate2Git(t, w.Path, "rev-parse", "HEAD"); got != head || gate2Git(t, w.Path, "rev-parse", "refs/heads/"+w.BranchName) != head ||
				gate2Git(t, w.Path, "symbolic-ref", "HEAD") != "refs/heads/"+w.BranchName {
				t.Fatalf("restoration moved previous branch: got=%s want=%s", got, head)
			}
			body, err := os.ReadFile(filepath.Join(w.Path, "source"))
			if err != nil || string(body) != "previous harness commit" {
				t.Fatalf("previous committed work lost: %s %v", body, err)
			}
			body, err = os.ReadFile(filepath.Join(w.Path, "dummy config"))
			if err != nil || string(body) != newCopy || e.Claim.Worktree.SetupStatus != "pending" {
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
			if err == nil || e.WorktreePath != "" ||
				failure != "ack" && (e.WorktreeID != "" || e.BaseSHA != "") ||
				failure == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("reuse proceeded without state/ACK: err=%v id=%s path=%s base=%s", err, e.WorktreeID, e.WorktreePath, e.BaseSHA)
			}
		})
	}
}

// Reporting a new observed HEAD must never replace the last accepted checkpoint.
// Exercise owner retry and authenticated claim rather than manufacturing seq 3.
func TestContinueBlockedOrFailedKeepsAdvancedCheckpointForRetryHTTP(t *testing.T) {
	for _, stop := range []string{"blocked", "failed"} {
		for _, known := range []string{"harness", "pushed"} {
			t.Run(stop+"/"+known, func(t *testing.T) {
				f, e, r, _ := newContinueExecution(t)
				w := e.Claim.Worktree
				gate2Write(t, filepath.Join(w.Path, "source"), "previous harness commit")
				gate2Git(t, w.Path, "add", "source")
				gate2Git(t, w.Path, "commit", "-m", "previous work")
				head := gate2Git(t, w.Path, "rev-parse", "HEAD")
				if head == w.BaseSHA {
					t.Fatal("test requires an advanced checkpoint")
				}
				if _, err := f.db.Exec(`UPDATE executions SET harness_head_sha=? WHERE id=?`, head, logExecution); err != nil {
					t.Fatal(err)
				}
				if known == "pushed" {
					if _, err := f.db.Exec(`UPDATE executions SET harness_head_sha=base_sha,verified_sha=?,pushed_sha=? WHERE id=?`, head, head, logExecution); err != nil {
						t.Fatal(err)
					}
				}
				var reuser WorktreeReuser = r
				if stop == "blocked" {
					gate2Write(t, filepath.Join(w.Path, "source"), "uncommitted change")
				} else {
					reuser = failingWorktreeReuser{Repositories: r}
				}
				err := e.ReuseWorktree(context.Background(), reuser, store.WorktreeReuseStore{DB: f.db}, reuseAudit(t))
				if err == nil || stop == "blocked" && !errors.Is(err, worktree.ErrGitState) {
					t.Fatalf("expected %s: %v", stop, err)
				}
				var status, checkpoint, base, id string
				if err := f.db.QueryRow(`SELECT status,harness_head_sha,base_sha,worktree_id FROM executions WHERE id=?`, e.Claim.Execution.ID).
					Scan(&status, &checkpoint, &base, &id); err != nil || status != stop || checkpoint != head || base != w.BaseSHA || id != w.ID {
					t.Fatalf("stop lost checkpoint: status=%s checkpoint=%s base=%s id=%s err=%v", status, checkpoint, base, id, err)
				}
				if stop == "blocked" {
					gate2Git(t, w.Path, "restore", "source")
				}
				claim, process := retryContinueWorktreeHTTP(t, f)
				// Inheritance is already durable while pending/claimed, before
				// preparation can fail or receive another cancellation.
				if err := f.db.QueryRow(`SELECT harness_head_sha,base_sha,worktree_id FROM executions WHERE id=?`, claim.Execution.ID).
					Scan(&checkpoint, &base, &id); err != nil || checkpoint != head || base != w.BaseSHA || id != w.ID {
					t.Fatalf("retry lost checkpoint: %s %s %s %v", checkpoint, base, id, err)
				}
				if err := process.ReportResult(context.Background(), claim.Execution.ID, ExecutionResult{FromStatus: model.ExecutionClaimed, Status: model.ExecutionPreparing}); err != nil {
					t.Fatal(err)
				}
				control := testExecutionControl()
				control.Reporter = process
				next := &Execution{Claim: *claim, control: &control, status: model.ExecutionPreparing}
				if err := next.ReuseWorktree(context.Background(), r, store.WorktreeReuseStore{DB: f.db}, reuseAudit(t)); err != nil {
					t.Fatalf("retry rejected the accepted checkpoint: %v", err)
				}
				assertReusedWorktree(t, f, next, "succeeded")
				if next.HarnessHeadSHA != head || gate2Git(t, w.Path, "rev-parse", "HEAD") != head {
					t.Fatal("retry changed checkpoint")
				}
			})
		}
	}
}

type failingWorktreeReuser struct{ worktree.Repositories }

func (r failingWorktreeReuser) Reuse(context.Context, worktree.ReuseRequest) (worktree.Reused, error) {
	return worktree.Reused{}, errors.New("injected Git I/O failure")
}

func TestContinueRequiresPreparationEvidenceHTTP(t *testing.T) {
	for _, evidence := range []string{"missing-ready", "wrong-ready-path", "setup-pending", "setup-failed", "setup-not-configured", "missing-setup-skip", "incomplete-restore", "late-old-ready"} {
		t.Run(evidence, func(t *testing.T) {
			f, e, r, _ := newContinueExecution(t)
			w := e.Claim.Worktree
			switch evidence {
			case "missing-ready":
				if _, err := f.db.Exec(`DELETE FROM execution_events WHERE type='worktree_ready'`); err != nil {
					t.Fatal(err)
				}
			case "wrong-ready-path":
				if _, err := f.db.Exec(`UPDATE execution_events SET payload=json_set(payload,'$.path','/different') WHERE type='worktree_ready'`); err != nil {
					t.Fatal(err)
				}
			case "setup-pending":
				if _, err := f.db.Exec(`UPDATE projects SET setup_command='test setup'; UPDATE worktrees SET setup_status='pending'`); err != nil {
					t.Fatal(err)
				}
			case "setup-failed":
				if _, err := f.db.Exec(`UPDATE worktrees SET setup_status='failed'`); err != nil {
					t.Fatal(err)
				}
			case "missing-setup-skip":
				if _, err := f.db.Exec(`UPDATE worktrees SET setup_status='pending'; DELETE FROM execution_events WHERE type='log' AND json_extract(payload,'$.sourceId') LIKE 'worktree-setup-not-required/%'`); err != nil {
					t.Fatal(err)
				}
			case "setup-not-configured":
				if _, err := f.db.Exec(`UPDATE projects SET setup_command=NULL; UPDATE worktrees SET setup_status='pending'`); err != nil {
					t.Fatal(err)
				}
			case "incomplete-restore", "late-old-ready":
				history := store.WorktreeReuseStore{DB: f.db}
				if err := history.BeginWorktreeRestore(context.Background(), e.Claim.Execution.ID, w.ID); err != nil {
					t.Fatal(err)
				}
				// A stale succeeded flag alone is insufficient for completion.
				if _, err := f.db.Exec(`UPDATE worktrees SET setup_status='succeeded'`); err != nil {
					t.Fatal(err)
				}
				if evidence == "late-old-ready" {
					payload, _ := json.Marshal(map[string]string{"path": w.Path, "branch": w.BranchName, "baseSha": w.BaseSHA})
					if _, err := store.AppendExecutionEvent(context.Background(), f.db, logExecution, model.EventWorktreeReady, payload); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := e.ReuseWorktree(context.Background(), r, store.WorktreeReuseStore{DB: f.db}, reuseAudit(t))
			if evidence == "setup-not-configured" {
				if err != nil {
					t.Fatal(err)
				}
				assertReusedWorktree(t, f, e, "pending")
				return
			}
			if !errors.Is(err, worktree.ErrGitState) || e.WorktreePath != "" {
				t.Fatalf("missing preparation evidence accepted: %v", err)
			}
			var cause string
			if err := f.db.QueryRow(`SELECT cause FROM execution_blocks WHERE execution_id=?`, e.Claim.Execution.ID).Scan(&cause); err != nil || cause != "git_state" {
				t.Fatalf("cause=%s err=%v", cause, err)
			}
		})
	}
}

func TestContinueInterruptedSetupCannotBeSkippedByRemovingCommandHTTP(t *testing.T) {
	for _, outcome := range []string{"cancelled", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			f, e, r, source := newContinueExecution(t)
			w := e.Claim.Worktree
			if _, err := f.db.Exec(`UPDATE projects SET setup_command='test setup',local_repo_path=?`, source); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(w.Path); err != nil {
				t.Fatal(err)
			}
			if err := e.ReuseWorktree(context.Background(), r, store.WorktreeReuseStore{DB: f.db}, reuseAudit(t)); err != nil {
				t.Fatal(err)
			}
			assertReusedWorktree(t, f, e, "pending")
			// Simulate an interruption after copy/activation, before setup can
			// record success. Removing today's command must not fabricate proof
			// that the previous generation finished preparation.
			class := model.ErrorCancelled
			status := model.ExecutionCancelled
			if outcome == "failed" {
				class = model.ErrorConfig
				status = model.ExecutionFailed
			}
			if err := e.report(status, class); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE projects SET setup_command=NULL`); err != nil {
				t.Fatal(err)
			}
			claim, process := retryContinueWorktreeHTTP(t, f)
			if err := process.ReportResult(context.Background(), claim.Execution.ID, ExecutionResult{FromStatus: model.ExecutionClaimed, Status: model.ExecutionPreparing}); err != nil {
				t.Fatal(err)
			}
			control := testExecutionControl()
			control.Reporter = process
			next := &Execution{Claim: *claim, control: &control, status: model.ExecutionPreparing}
			if err := next.ReuseWorktree(context.Background(), r, store.WorktreeReuseStore{DB: f.db}, reuseAudit(t)); !errors.Is(err, worktree.ErrGitState) {
				t.Fatalf("unfinished setup was accepted: %v", err)
			}
			var setup, manifest string
			if err := f.db.QueryRow(`SELECT setup_status,copied_files FROM worktrees WHERE id=?`, w.ID).Scan(&setup, &manifest); err != nil || setup != "pending" {
				t.Fatalf("setup state changed: %s %v", setup, err)
			}
			var copies []model.CopiedFile
			if err := json.Unmarshal([]byte(manifest), &copies); err != nil || len(copies) != 1 {
				t.Fatalf("copy manifest lost: %s %v", manifest, err)
			}
			body, err := os.ReadFile(filepath.Join(w.Path, copies[0].Path))
			if err != nil || string(body) != "dummy test value\n" || copies[0].SHA256 != fmt.Sprintf("%x", sha256.Sum256(body)) {
				t.Fatalf("saved restored content/hash differs: %s %+v %v", body, copies, err)
			}
		})
	}
}
