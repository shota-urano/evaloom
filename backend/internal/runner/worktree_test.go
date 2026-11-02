package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/safegit"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

type pathAuditFunc func(context.Context, string) error

func (f pathAuditFunc) RecordWorktreePathRejection(ctx context.Context, id string) error {
	return f(ctx, id)
}

type createWorktreeFunc func(context.Context, worktree.CreateRequest) (worktree.Created, error)

func (f createWorktreeFunc) Create(ctx context.Context, request worktree.CreateRequest) (worktree.Created, error) {
	return f(ctx, request)
}

// The creation, authenticated reports and DB transitions use production code.
// Only the unspecified audit delivery boundary is supplied by the caller.
func newCreationExecution(t *testing.T) (*logControlPlane, *Execution, worktree.Repositories, string, string, string) {
	t.Helper()
	f := newLogControlPlane(t)
	root := t.TempDir()
	seed, remote := filepath.Join(root, "seed"), filepath.Join(root, "remote.git")
	gate2Git(t, root, "init", "--initial-branch=main", seed)
	gate2Write(t, filepath.Join(seed, "source"), "initial")
	gate2Git(t, seed, "add", ".")
	gate2Git(t, seed, "commit", "-m", "initial")
	gate2Git(t, root, "init", "--bare", "--initial-branch=main", remote)
	gate2Git(t, remote, "fetch", seed, "HEAD:refs/heads/main")
	files := http.FileServer(http.Dir(remote))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info/refs":
			out, err := safegit.Output(r.Context(), remote, "for-each-ref", "--format=%(objectname)%09%(refname)")
			if err != nil {
				http.Error(w, "refs", 500)
				return
			}
			_, _ = w.Write(out)
		case "/objects/info/packs":
			_, _ = w.Write([]byte("\n"))
		default:
			files.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	if _, err := f.db.Exec(`UPDATE projects SET git_remote_url=?; UPDATE executions SET status='preparing'`, server.URL); err != nil {
		t.Fatal(err)
	}
	p, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.GetProject(context.Background(), f.db, "project")
	if err != nil {
		t.Fatal(err)
	}
	control := testExecutionControl()
	control.Reporter = p
	e := &Execution{control: &control, status: model.ExecutionPreparing}
	e.Claim.Execution.ID, e.Claim.Execution.TaskID, e.Claim.Execution.Mode = logExecution, "task-001", "continue"
	e.Claim.Execution.Seq, e.Claim.Task.ShortName, e.Claim.Project = 1, "logs", project
	r := worktree.Repositories{WorktreeRoot: f.config.WorktreeRoot}
	repository, err := r.Prepare(context.Background(), project.ID, project.GitRemoteURL)
	if err != nil {
		t.Fatal(err)
	}
	return f, e, r, repository, seed, remote
}

func TestCreateWorktreeHTTPAcceptanceNormalAndFresh(t *testing.T) {
	for _, mode := range []string{"continue", "fresh"} {
		t.Run(mode, func(t *testing.T) {
			f, e, r, repository, _, _ := newCreationExecution(t)
			e.Claim.Execution.Mode = mode
			ordinal, suffix := 0, ""
			if mode == "fresh" {
				// Execution 9 is the second fresh, not r9. Continue attempts
				// must not affect the naming counter.
				e.Claim.Execution.Seq, ordinal, suffix = 9, 2, "-r2"
			}
			if err := e.RecordRemoteBase(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			if err := e.CreateWorktree(context.Background(), r, ordinal, pathAuditFunc(func(context.Context, string) error {
				t.Fatal("successful creation must not record a rejection")
				return nil
			})); err != nil {
				t.Fatal(err)
			}
			wantBranch := "evaloom/task-001-logs" + suffix
			if e.BranchName != wantBranch || gate2Git(t, e.WorktreePath, "symbolic-ref", "HEAD") != "refs/heads/"+wantBranch ||
				gate2Git(t, repository, "rev-parse", "refs/heads/"+wantBranch) != e.BaseSHA {
				t.Fatalf("prepared execution branch=%s base=%s path=%s", e.BranchName, e.BaseSHA, e.WorktreePath)
			}
			root, err := filepath.EvalSymlinks(r.WorktreeRoot)
			if err != nil {
				t.Fatal(err)
			}
			real, err := filepath.EvalSymlinks(e.WorktreePath)
			if err != nil {
				t.Fatal(err)
			}
			rel, err := filepath.Rel(root, real)
			if err != nil || rel != "task-001"+suffix || real != e.WorktreePath {
				t.Fatalf("worktree real path=%s root=%s relative=%s err=%v", real, root, rel, err)
			}
			// Existing transition body already accepts branchName. Check that
			// the next progress report actually persists the selected branch.
			if err := e.report(model.ExecutionRunning, ""); err != nil {
				t.Fatal(err)
			}
			var branch, base string
			if err := f.db.QueryRow(`SELECT branch_name,base_sha FROM executions WHERE id=?`, logExecution).Scan(&branch, &base); err != nil || branch != wantBranch || base != e.BaseSHA {
				t.Fatalf("saved branch=%s base=%s err=%v", branch, base, err)
			}
		})
	}
}

func TestCreateWorktreeHTTPCollisionFailsAndPreservesExistingSHA(t *testing.T) {
	for _, place := range []string{"local", "remote", "fresh"} {
		t.Run(place, func(t *testing.T) {
			f, e, r, repository, seed, remote := newCreationExecution(t)
			old := gate2Git(t, repository, "rev-parse", "HEAD")
			gate2Write(t, filepath.Join(seed, "source"), "new base")
			gate2Git(t, seed, "add", ".")
			gate2Git(t, seed, "commit", "-m", "new base")
			gate2Git(t, remote, "fetch", seed, "HEAD:refs/heads/main")
			ordinal, ref := 0, "refs/heads/evaloom/task-001-logs"
			if place == "fresh" {
				e.Claim.Execution.Mode, ordinal, ref = "fresh", 2, ref+"-r2"
			}
			if err := e.RecordRemoteBase(context.Background(), r); err != nil || old == e.BaseSHA {
				t.Fatalf("fixture requires distinct start/existing commits: %v", err)
			}
			where := repository
			if place == "remote" {
				where = remote
			}
			// Safegit has no ref-mutating builtin; a fixture fetch creates the
			// colliding branch at the OLD commit after the Base Branch fetch.
			gate2Git(t, where, "fetch", seed, old+":"+ref)
			err := e.CreateWorktree(context.Background(), r, ordinal, pathAuditFunc(func(context.Context, string) error { return nil }))
			if err == nil || e.WorktreePath != "" || e.BranchName != "" || gate2Git(t, where, "rev-parse", ref) != old {
				t.Fatalf("collision err=%v branch=%s path=%s", err, e.BranchName, e.WorktreePath)
			}
			var status, class string
			if err := f.db.QueryRow(`SELECT status,error_class FROM executions WHERE id=?`, logExecution).Scan(&status, &class); err != nil || status != "failed" || class != "config_error" {
				t.Fatalf("saved status=%s class=%s err=%v", status, class, err)
			}
		})
	}
}

func TestCreateWorktreePathEscapeRequiresAuditAndFails(t *testing.T) {
	f, e, r, _, _, _ := newCreationExecution(t)
	if err := e.RecordRemoteBase(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(r.WorktreeRoot, "task-001")); err != nil {
		t.Fatal(err)
	}
	audits := 0
	err := e.CreateWorktree(context.Background(), r, 0, pathAuditFunc(func(ctx context.Context, id string) error {
		audits++
		if ctx != e.control.ReportContext || id != logExecution {
			t.Fatalf("audit context/identity mismatch: %s", id)
		}
		return nil
	}))
	if err == nil || audits != 1 || e.WorktreePath != "" {
		t.Fatalf("escape err=%v audits=%d path=%s", err, audits, e.WorktreePath)
	}
	var status, class string
	if err := f.db.QueryRow(`SELECT status,error_class FROM executions WHERE id=?`, logExecution).Scan(&status, &class); err != nil || status != "failed" || class != "config_error" {
		t.Fatalf("saved status=%s class=%s err=%v", status, class, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("path rejection wrote outside root: %v %v", entries, err)
	}
}

func TestCreateWorktreeGuardsAndRedactsErrors(t *testing.T) {
	for _, name := range []string{"reuse", "missing-snapshot", "missing-base", "missing-ordinal", "unexpected-ordinal", "cancelled", "git-failure", "audit-failure"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			control := testExecutionControl()
			reports := 0
			control.Reporter = reportFunc(func(_ context.Context, _ string, result ExecutionResult) error {
				reports++
				if result.Status != model.ExecutionFailed || result.ErrorClass != model.ErrorConfig || strings.Contains(result.ErrorMessage, "secret") {
					t.Fatalf("unsafe failure report: %+v", result)
				}
				return nil
			})
			e := &Execution{control: &control, status: model.ExecutionPreparing, BaseSHA: strings.Repeat("a", 40)}
			e.Claim.Execution.Mode = "continue"
			ordinal := 0
			switch name {
			case "reuse":
				e.Claim.Worktree = &ClaimWorktree{}
			case "missing-snapshot":
				e.Claim.Execution.Seq = 2
			case "missing-base":
				e.BaseSHA = ""
			case "missing-ordinal":
				e.Claim.Execution.Mode = "fresh"
			case "unexpected-ordinal":
				ordinal = 1
			case "cancelled":
				cancel()
			}
			calls, audits := 0, 0
			err := e.CreateWorktree(ctx, createWorktreeFunc(func(context.Context, worktree.CreateRequest) (worktree.Created, error) {
				calls++
				if name == "audit-failure" {
					return worktree.Created{}, worktree.ErrWorktreePath
				}
				return worktree.Created{}, errors.New("credential=secret")
			}), ordinal, pathAuditFunc(func(context.Context, string) error {
				audits++
				return errors.New("audit credential=secret")
			}))
			if err == nil || strings.Contains(err.Error(), "secret") || e.WorktreePath != "" {
				t.Fatalf("err=%v path=%s", err, e.WorktreePath)
			}
			if name == "git-failure" || name == "audit-failure" {
				if reports != 1 || calls != 1 || name == "audit-failure" && audits != 1 {
					t.Fatalf("reports=%d calls=%d audits=%d", reports, calls, audits)
				}
			} else if reports != 0 || calls != 0 {
				t.Fatalf("guard triggered side effects: reports=%d calls=%d", reports, calls)
			}
		})
	}
}
