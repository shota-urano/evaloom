package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/safegit"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

type baseFetchFunc func(context.Context, string, string, string) (string, error)

func (f baseFetchFunc) FetchBase(ctx context.Context, project, url, branch string) (string, error) {
	return f(ctx, project, url, branch)
}

// Only the remote transport is local HTTP. Fetch, safe Git, authenticated result
// delivery and SQLite persistence all use their production implementations.
func TestRecordRemoteBaseHTTPAcceptance(t *testing.T) {
	f := newLogControlPlane(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	seed, remote, owner := filepath.Join(root, "seed"), filepath.Join(root, "remote.git"), filepath.Join(root, "owner")
	gate2Git(t, root, "init", "--initial-branch=main", seed)
	gate2Write(t, filepath.Join(seed, "source"), "initial")
	gate2Git(t, seed, "add", ".")
	gate2Git(t, seed, "commit", "-m", "initial")
	gate2Git(t, root, "init", "--bare", "--initial-branch=main", remote)
	gate2Git(t, remote, "fetch", seed, "HEAD:refs/heads/main")
	gate2Git(t, root, "clone", "--no-local", remote, owner)
	gate2Git(t, owner, "switch", "-c", "personal-work")
	gate2Write(t, filepath.Join(owner, "source"), "staged")
	gate2Git(t, owner, "add", "source")
	gate2Write(t, filepath.Join(owner, "source"), "unstaged")
	gate2Write(t, filepath.Join(owner, "untracked"), "keep me")
	before := baseOwnerSnapshot(t, owner)
	localSHA := gate2Git(t, owner, "rev-parse", "refs/heads/main")
	files := http.FileServer(http.Dir(remote))
	remoteHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info/refs" {
			out, err := safegit.Output(r.Context(), remote, "for-each-ref", "--format=%(objectname)%09%(refname)")
			if err != nil {
				http.Error(w, "refs", 500)
				return
			}
			_, _ = w.Write(out)
			return
		}
		if r.URL.Path == "/objects/info/packs" {
			_, _ = w.Write([]byte("\n"))
			return
		}
		files.ServeHTTP(w, r)
	}))
	defer remoteHTTP.Close()
	repositories := worktree.Repositories{WorktreeRoot: f.config.WorktreeRoot}
	if _, err := repositories.Prepare(ctx, "project", remoteHTTP.URL); err != nil {
		t.Fatal(err)
	}
	// Advance the remote AFTER clone preparation. Local main and the clone's
	// tracking ref must be stale, so the test cannot pass without a new fetch.
	gate2Write(t, filepath.Join(seed, "source"), "remote update")
	gate2Git(t, seed, "add", ".")
	gate2Git(t, seed, "commit", "-m", "remote update")
	gate2Git(t, remote, "fetch", seed, "HEAD:refs/heads/main")
	want := gate2Git(t, remote, "rev-parse", "refs/heads/main")
	if want == localSHA {
		t.Fatal("remote must differ from local Base Branch")
	}
	if _, err := f.db.Exec(`UPDATE projects SET local_repo_path=?,git_remote_url=?; UPDATE executions SET status='preparing'`, owner, remoteHTTP.URL); err != nil {
		t.Fatal(err)
	}
	p, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.GetProject(ctx, f.db, "project")
	if err != nil {
		t.Fatal(err)
	}
	control := testExecutionControl()
	control.Reporter = p
	e := &Execution{control: &control, status: model.ExecutionPreparing}
	e.Claim.Execution.ID, e.Claim.Execution.Mode, e.Claim.Project = logExecution, "fresh", project
	if err := e.RecordRemoteBase(ctx, repositories); err != nil {
		t.Fatal(err)
	}
	var saved, status string
	if err := f.db.QueryRow(`SELECT base_sha,status FROM executions WHERE id=?`, logExecution).Scan(&saved, &status); err != nil || saved != want || e.BaseSHA != saved || status != "preparing" {
		t.Fatalf("recorded base_sha=%s memory=%s remote=%s status=%s err=%v", saved, e.BaseSHA, want, status, err)
	}
	// Lost acknowledgement replay uses the existing result delivery contract.
	if err := p.ReportResult(ctx, logExecution, ExecutionResult{FromStatus: model.ExecutionPreparing, Status: model.ExecutionPreparing, BaseSHA: want}); err != nil {
		t.Fatal(err)
	}
	if after := baseOwnerSnapshot(t, owner); !reflect.DeepEqual(before, after) || gate2Git(t, owner, "symbolic-ref", "HEAD") != "refs/heads/personal-work" {
		t.Fatal("owner working tree, checkout, index, refs, config or objects changed")
	}
	if got := gate2Git(t, owner, "status", "--porcelain"); !strings.Contains(got, "MM source") || !strings.Contains(got, "?? untracked") {
		t.Fatalf("owner's uncommitted changes changed: %s", got)
	}
}

func baseOwnerSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[strings.TrimPrefix(path, root)] = string(data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRecordRemoteBaseStopsAndReportsConfigErrorOnFetchFailure(t *testing.T) {
	f := newLogControlPlane(t)
	if _, err := f.db.Exec(`UPDATE executions SET status='preparing'`); err != nil {
		t.Fatal(err)
	}
	p, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	control := testExecutionControl()
	control.Reporter = p
	e := &Execution{control: &control, status: model.ExecutionPreparing}
	e.Claim.Execution.ID, e.Claim.Execution.Mode = logExecution, "fresh"
	err = e.RecordRemoteBase(context.Background(), baseFetchFunc(func(context.Context, string, string, string) (string, error) {
		return "", errors.New("https://user:secret@remote: permission denied")
	}))
	if err == nil || strings.Contains(err.Error(), "secret") || e.BaseSHA != "" {
		t.Fatalf("fetch error or SHA: %v %s", err, e.BaseSHA)
	}
	var status, class, message, base string
	if err := f.db.QueryRow(`SELECT status,error_class,error_message,coalesce(base_sha,'') FROM executions WHERE id=?`, logExecution).Scan(&status, &class, &message, &base); err != nil ||
		status != "failed" || class != "config_error" || base != "" || strings.Contains(message, "secret") {
		t.Fatalf("failure report: %s %s %s %s %v", status, class, message, base, err)
	}
}

func TestRecordRemoteBaseRequiresAcknowledgementAndNeverRefetchesReuse(t *testing.T) {
	for _, name := range []string{"unacknowledged", "reuse", "missing-continue-snapshot", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			control := testExecutionControl()
			control.Reporter = reportFunc(func(context.Context, string, ExecutionResult) error { return errors.New("unacknowledged") })
			e := &Execution{control: &control, status: model.ExecutionPreparing}
			e.Claim.Execution.Mode = "fresh"
			if name == "reuse" || name == "missing-continue-snapshot" {
				e.Claim.Execution.Mode, e.Claim.Execution.Seq = "continue", 2
				if name == "reuse" {
					e.Claim.Worktree = &ClaimWorktree{BaseSHA: strings.Repeat("a", 40)}
				}
			}
			if name == "cancelled" {
				cancel()
			}
			calls := 0
			err := e.RecordRemoteBase(ctx, baseFetchFunc(func(context.Context, string, string, string) (string, error) {
				calls++
				return strings.Repeat("b", 40), nil
			}))
			if err == nil || e.BaseSHA != "" || (name == "reuse" || name == "missing-continue-snapshot") && calls != 0 || name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("err=%v SHA=%s fetches=%d", err, e.BaseSHA, calls)
			}
		})
	}
}
