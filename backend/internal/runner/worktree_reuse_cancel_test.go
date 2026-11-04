package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

// Cancel at a deterministic boundary inside restoration/copying, after real
// Git/file operations but before ReuseWorktree receives their result.
type cancellingWorktreeReuser struct {
	worktree.Repositories
	t      *testing.T
	phase  string
	cancel context.CancelFunc
	called bool
}

func (r *cancellingWorktreeReuser) Reuse(ctx context.Context, request worktree.ReuseRequest) (worktree.Reused, error) {
	if r.phase == "begin" {
		before := request.BeforeRecreate
		request.BeforeRecreate = func(ctx context.Context) error {
			if err := before(ctx); err != nil {
				return err
			}
			r.called = true
			r.cancel()
			return ctx.Err()
		}
		return r.Repositories.Reuse(ctx, request)
	}
	result, err := r.Repositories.Reuse(ctx, request)
	if err != nil || !result.Recreated {
		r.t.Fatalf("real restoration did not finish: %+v %v", result, err)
	}
	if r.phase == "restore" {
		r.called = true
		r.cancel()
		return result, ctx.Err()
	}
	return result, nil
}

func (r *cancellingWorktreeReuser) CopyFiles(ctx context.Context, source, destination string, allowlist []string, warn func(context.Context, string) error) ([]model.CopiedFile, error) {
	if r.phase == "partial-copy" {
		// Cancel inside the real copy loop, after the first atomic file publish
		// and before the second file. The warning provides a deterministic hook.
		files, err := r.Repositories.CopyFiles(ctx, source, destination, allowlist, func(ctx context.Context, path string) error {
			if err := warn(ctx, path); err != nil {
				return err
			}
			r.called = true
			r.cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) || len(files) != 1 || !r.called {
			r.t.Fatalf("real partial copy did not cancel: %+v %v", files, err)
		}
		return files, err
	}
	files, err := r.Repositories.CopyFiles(ctx, source, destination, allowlist, warn)
	if err != nil || len(files) != 1 {
		r.t.Fatalf("real restored copy did not finish: %+v %v", files, err)
	}
	r.called = true
	r.cancel()
	return files, ctx.Err()
}

func TestContinueWorktreeCancellationDuringRestoreRetainsAssociationForRetryHTTP(t *testing.T) {
	for _, removal := range []string{"cleaned", "manually-deleted"} {
		for _, phase := range []string{"begin", "restore", "copy", "partial-copy"} {
			t.Run(removal+"/"+phase, func(t *testing.T) {
				f, previous, repositories, source := newContinueExecution(t)
				w := previous.Claim.Worktree
				// Restored copies must use current source bytes, even when their
				// hashes differ from the removed directory's saved manifest.
				const content = "updated dummy test value\n"
				gate2Write(t, filepath.Join(source, "dummy config"), content)
				if phase == "partial-copy" {
					gate2Write(t, filepath.Join(source, "second config"), "second dummy value")
					previous.Claim.Project.CopyAllowlist = []string{"dummy config", "missing", "second config"}
				}
				gate2Write(t, filepath.Join(w.Path, "source"), "previous harness commit")
				gate2Git(t, w.Path, "add", "source")
				gate2Git(t, w.Path, "commit", "-m", "previous work")
				head := gate2Git(t, w.Path, "rev-parse", "HEAD")
				if head == w.BaseSHA {
					t.Fatal("test requires a branch ahead of base")
				}
				if _, err := f.db.Exec(`UPDATE executions SET harness_head_sha=? WHERE id=?`, head, logExecution); err != nil {
					t.Fatal(err)
				}
				if removal == "cleaned" {
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
				// The Executor owns cancellation reporting, using a context which
				// outlives the cancelled preparation stage.
				if _, err := f.db.Exec(`UPDATE executions SET status='claimed' WHERE id=?`, previous.Claim.Execution.ID); err != nil {
					t.Fatal(err)
				}
				allowlist, err := json.Marshal(previous.Claim.Project.CopyAllowlist)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE projects SET local_repo_path=?,copy_allowlist=?`, source, string(allowlist)); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				repositories.Transport = rejectReuseNetwork{t}
				reuser := &cancellingWorktreeReuser{Repositories: repositories, t: t, phase: phase, cancel: cancel}
				history := store.WorktreeReuseStore{DB: f.db}
				handlers := stubStageHandlers(func(stage StageID, ctx context.Context, e *Execution) error {
					switch stage {
					case StageValidateProject, StageCheckPreviousPR:
						return nil
					case StagePrepareWorktree:
						return e.ReuseWorktree(ctx, reuser, history, reuseAudit(t))
					default:
						t.Fatalf("stage %s ran after cancelled restoration", stage)
						return nil
					}
				})
				executor, err := NewExecutorWithControl(handlers, *previous.control)
				if err != nil {
					t.Fatal(err)
				}
				err = executor.Execute(ctx, previous.Claim)
				if !errors.Is(err, context.Canceled) || !reuser.called {
					t.Fatalf("expected cancellation inside %s: %v called=%v", phase, err, reuser.called)
				}
				var status, class, taskStatus string
				var id, base, branch, savedHead *string
				if err := f.db.QueryRow(`SELECT e.status,e.error_class,t.status,e.worktree_id,e.base_sha,e.branch_name,e.harness_head_sha
					FROM executions e JOIN tasks t ON t.id=e.task_id WHERE e.id=?`, previous.Claim.Execution.ID).
					Scan(&status, &class, &taskStatus, &id, &base, &branch, &savedHead); err != nil ||
					status != "cancelled" || class != "cancelled" || taskStatus != "cancelled" ||
					id == nil || *id != w.ID || base == nil || *base != w.BaseSHA || branch == nil || *branch != w.BranchName || savedHead == nil || *savedHead != head {
					t.Fatalf("cancelled DB checkpoint: status=%s class=%s task=%s id=%v base=%v branch=%v head=%v err=%v", status, class, taskStatus, id, base, branch, savedHead, err)
				}
				var worktreeStatus string
				var cleaned *string
				if err := f.db.QueryRow(`SELECT status,cleaned_at FROM worktrees WHERE id=?`, w.ID).Scan(&worktreeStatus, &cleaned); err != nil ||
					worktreeStatus != "cleaned" || cleaned == nil {
					t.Fatalf("cancel activated the worktree: %s cleaned=%v err=%v", worktreeStatus, cleaned, err)
				}
				claim, process := retryContinueWorktreeHTTP(t, f)
				if claim.Execution.Seq != 3 || claim.Worktree == nil || claim.Worktree.ID != w.ID || claim.Worktree.Path != w.Path ||
					claim.Worktree.BaseSHA != w.BaseSHA || claim.Worktree.BranchName != w.BranchName {
					t.Fatalf("retry lost worktree: %+v", claim)
				}
				if err := process.ReportResult(context.Background(), claim.Execution.ID, ExecutionResult{
					FromStatus: model.ExecutionClaimed, Status: model.ExecutionPreparing,
				}); err != nil {
					t.Fatal(err)
				}
				control := testExecutionControl()
				control.Reporter = process
				next := &Execution{Claim: *claim, control: &control, status: model.ExecutionPreparing}
				// Missing preparation evidence blocks BEFORE Git or copying. The
				// owner source changes again; retry must not overwrite files.
				before, readErr := os.ReadFile(filepath.Join(w.Path, "dummy config"))
				gate2Write(t, filepath.Join(source, "dummy config"), "must not be copied on retry")
				if err := next.ReuseWorktree(context.Background(), repositories, history, reuseAudit(t)); !errors.Is(err, worktree.ErrGitState) {
					t.Fatalf("unfinished preparation did not block: %v", err)
				}
				var cause, checkpoint, setup, manifest string
				if err := f.db.QueryRow(`SELECT b.cause,e.harness_head_sha,w.setup_status,w.copied_files
                    FROM executions e JOIN execution_blocks b ON b.execution_id=e.id
                    JOIN worktrees w ON w.id=e.worktree_id WHERE e.id=?`, claim.Execution.ID).
					Scan(&cause, &checkpoint, &setup, &manifest); err != nil ||
					cause != "git_state" || checkpoint != head || setup != "pending" || manifest != "[]" {
					t.Fatalf("unfinished restore snapshot: cause=%s head=%s setup=%s manifest=%s err=%v", cause, checkpoint, setup, manifest, err)
				}
				after, afterErr := os.ReadFile(filepath.Join(w.Path, "dummy config"))
				if string(before) != string(after) || errors.Is(readErr, os.ErrNotExist) != errors.Is(afterErr, os.ErrNotExist) {
					t.Fatalf("retry modified incomplete files: before=%q after=%q errors=%v/%v", before, after, readErr, afterErr)
				}
				if phase == "begin" || phase == "restore" {
					if !errors.Is(afterErr, os.ErrNotExist) {
						t.Fatalf("copy ran too early: %q %v", after, afterErr)
					}
				} else if afterErr != nil || string(after) != content {
					t.Fatalf("completed copy prefix lost: %q %v", after, afterErr)
				}
				if phase == "begin" {
					if _, err := os.Lstat(w.Path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("retry recreated incomplete worktree: %v", err)
					}
				} else if got := gate2Git(t, w.Path, "rev-parse", "HEAD"); got != head || gate2Git(t, w.Path, "rev-parse", "refs/heads/"+w.BranchName) != head {
					t.Fatalf("retry moved branch: head=%s want=%s", got, head)
				}
				if next.WorktreePath != "" {
					t.Fatal("blocked worktree was made available to later stages")
				}

			})
		}
	}
}

// Use the real owner retry and authenticated Runner claim routes. SQL only
// supplies the Registry inputs absent from the worktree preparation fixture.
func retryContinueWorktreeHTTP(t *testing.T, f *logControlPlane) (*Claim, *Runner) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO harnesses (id,name,specialty,supported_tasks,adapter,repo_path,repo_url,supported_models,available_tools,tool_permissions,contract_version,created_at,updated_at)
		VALUES ('h','Harness','developer','["development"]','codex','/h','','[]','[]','{}','1','now','now')`,
		`INSERT INTO harness_profiles (id,harness_id,version,commit_sha,tag_name,definition,definition_hash,model_id,operation_status,registered_at)
		VALUES ('p','h','1.0.0','` + strings.Repeat("a", 40) + `','v1','{}','` + strings.Repeat("b", 64) + `','test','provisional','now')`,
		`INSERT INTO project_harnesses VALUES ('project','developer','h','p','now')`,
	} {
		if _, err := f.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequest("GET", f.server.URL+"/api/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(f.cookie)
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var session struct{ CSRFToken string }
	err = json.NewDecoder(response.Body).Decode(&session)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || session.CSRFToken == "" {
		t.Fatalf("session: status=%d err=%v", response.StatusCode, err)
	}
	request, err = http.NewRequest("POST", f.server.URL+"/api/tasks/task-001/retry", strings.NewReader(`{"mode":"continue"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(f.cookie)
	request.Header.Set("Origin", f.server.URL)
	request.Header.Set("X-CSRF-Token", session.CSRFToken)
	request.Header.Set("Content-Type", "application/json")
	response, err = f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusNoContent {
		t.Fatalf("retry: status=%d body=%s err=%v", response.StatusCode, body, err)
	}
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := process.claim(context.Background())
	if err != nil || claim == nil {
		t.Fatalf("retry claim=%+v err=%v", claim, err)
	}
	return claim, process
}

func TestContinueCancelledCleanedWorktreeValidatesPredecessorHTTP(t *testing.T) {
	for _, change := range []string{"base", "branch", "worktree", "fresh", "predecessor", "runner", "worktree-runner", "running"} {
		t.Run(change, func(t *testing.T) {
			f, e, _, _ := newContinueExecution(t)
			w := e.Claim.Worktree
			if _, err := f.db.Exec(`UPDATE worktrees SET status='cleaned',cleaned_at='now'`); err != nil {
				t.Fatal(err)
			}
			result := ExecutionResult{FromStatus: model.ExecutionPreparing, Status: model.ExecutionCancelled,
				ErrorClass: model.ErrorCancelled, WorktreeID: w.ID, BaseSHA: w.BaseSHA, BranchName: w.BranchName}
			switch change {
			case "base":
				result.BaseSHA = strings.Repeat("a", 40)
			case "branch":
				result.BranchName += "-other"
			case "worktree":
				result.WorktreeID = ulid.Make().String()
			case "fresh":
				if _, err := f.db.Exec(`UPDATE executions SET mode='fresh' WHERE id=?`, e.Claim.Execution.ID); err != nil {
					t.Fatal(err)
				}
			case "predecessor":
				if _, err := f.db.Exec(`UPDATE executions SET worktree_id=NULL WHERE id=?`, logExecution); err != nil {
					t.Fatal(err)
				}
			case "runner":
				if _, err := f.db.Exec(`UPDATE executions SET runner_id=NULL WHERE id=?`, e.Claim.Execution.ID); err != nil {
					t.Fatal(err)
				}
			case "worktree-runner":
				if _, err := runnerauth.Register(context.Background(), f.db, "other", "Other Runner", filepath.Join(f.config.WorktreeRoot, "other")); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE worktrees SET runner_id='other' WHERE id=?`, w.ID); err != nil {
					t.Fatal(err)
				}
			case "running":
				result.FromStatus = model.ExecutionRunning
				if _, err := f.db.Exec(`UPDATE executions SET status='running' WHERE id=?`, e.Claim.Execution.ID); err != nil {
					t.Fatal(err)
				}
			}
			err := e.control.Reporter.ReportResult(context.Background(), e.Claim.Execution.ID, result)
			code := "HTTP 422"
			if change == "runner" {
				code = "HTTP 403"
			}
			if err == nil || !strings.Contains(err.Error(), code) {
				t.Fatalf("invalid cancelled association accepted: %v", err)
			}
			var untouched bool
			if err := f.db.QueryRow(`SELECT e.worktree_id IS NULL AND e.base_sha IS NULL AND e.branch_name IS NULL
				AND w.status='cleaned' AND w.cleaned_at='now' FROM executions e JOIN worktrees w ON w.id=? WHERE e.id=?`, w.ID, e.Claim.Execution.ID).
				Scan(&untouched); err != nil || !untouched {
				t.Fatalf("rejected report changed checkpoint: %v %v", untouched, err)
			}
		})
	}
}
