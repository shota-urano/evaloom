package runner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/gate"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/worktree"
)

// Only the unspecified Runner/Control Plane delivery is bridged in-process.
// Fetch, creation, copy, persistence, and later Git inspection use real code.
func newCopyExecution(t *testing.T) (*logControlPlane, *Execution, worktree.Repositories, string) {
	t.Helper()
	f, e, r, _, source, _ := newCreationExecution(t)
	e.Claim.Project.LocalRepoPath = source
	if err := e.RecordRemoteBase(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := e.CreateWorktree(context.Background(), r, 0, pathAuditFunc(func(context.Context, string) error { return nil })); err != nil {
		t.Fatal(err)
	}
	return f, e, r, source
}

func TestCopyWorktreeFilesPersistsPathsAndActualSHA256(t *testing.T) {
	f, e, r, source := newCopyExecution(t)
	gate2Write(t, filepath.Join(source, ".env"), "unlisted dummy only")
	if err := os.MkdirAll(filepath.Join(source, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	gate2Write(t, filepath.Join(source, "config", "dummy.json"), "{\"mode\":\"test\"}\n")
	gate2Write(t, filepath.Join(source, "empty"), "")
	e.Claim.Project.CopyAllowlist = []string{"config/dummy.json", "empty", "missing"}
	before := gate2Git(t, source, "status", "--porcelain")
	recorder := store.WorktreeCopyStore{DB: f.db}
	if err := e.CopyWorktreeFiles(context.Background(), r, recorder); err != nil {
		t.Fatal(err)
	}
	if before != gate2Git(t, source, "status", "--porcelain") {
		t.Fatal("owner worktree changed")
	}
	var manifest, path, branch, base, status, setup, executionWorktree string
	if err := f.db.QueryRow(`SELECT w.copied_files,w.path,w.branch_name,w.base_sha,w.status,w.setup_status,e.worktree_id
		FROM worktrees w JOIN executions e ON e.worktree_id=w.id WHERE e.id=?`, logExecution).
		Scan(&manifest, &path, &branch, &base, &status, &setup, &executionWorktree); err != nil {
		t.Fatal(err)
	}
	var saved []model.CopiedFile
	if err := json.Unmarshal([]byte(manifest), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[0].Path != "config/dummy.json" || saved[1].Path != "empty" || !reflect.DeepEqual(saved, e.CopiedFiles) ||
		path != e.WorktreePath || base != e.BaseSHA || branch != e.BranchName || executionWorktree != e.WorktreeID || status != "active" || setup != "pending" {
		t.Fatalf("saved manifest=%s path=%s branch=%s base=%s status=%s setup=%s id=%s", manifest, path, branch, base, status, setup, executionWorktree)
	}
	for _, file := range saved {
		body, err := os.ReadFile(filepath.Join(path, file.Path))
		if err != nil || file.SHA256 != fmt.Sprintf("%x", sha256.Sum256(body)) {
			t.Fatalf("saved SHA256 disagrees with real file: %+v err=%v", file, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(path, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unlisted .env copied: %v", err)
	}
	var warning string
	if err := f.db.QueryRow(`SELECT payload FROM execution_events WHERE execution_id=? AND type='log'`, logExecution).Scan(&warning); err != nil ||
		!strings.Contains(warning, "missing") || !strings.Contains(warning, "警告") || strings.Contains(warning, source) {
		t.Fatalf("missing-source log=%s err=%v", warning, err)
	}
	prepared := model.PreparedWorktree{Path: path, BranchName: branch, BaseSHA: base, CopiedFiles: saved}
	if id, err := recorder.RecordCopiedWorktree(context.Background(), logExecution, prepared); err != nil || id != e.WorktreeID {
		t.Fatalf("identical record retry id=%s err=%v", id, err)
	}
	prepared.CopiedFiles = append([]model.CopiedFile(nil), saved...)
	prepared.CopiedFiles[0].SHA256 = strings.Repeat("0", 64)
	if _, err := recorder.RecordCopiedWorktree(context.Background(), logExecution, prepared); err == nil {
		t.Fatal("saved hashes were replaceable")
	}
	// Gate 3 consumes the actual manifest, including nested untracked files.
	if _, clean, err := gate.CheckWorktree(context.Background(), path, saved); err != nil || !clean {
		t.Fatalf("copied file exemptions: clean=%v err=%v", clean, err)
	}
	gate2Write(t, filepath.Join(path, saved[0].Path), "changed")
	if _, clean, err := gate.CheckWorktree(context.Background(), path, saved); err != nil || clean {
		t.Fatalf("modified copy must lose exemption: clean=%v err=%v", clean, err)
	}
}

func TestCopyWorktreeFilesSkipsSymlinksAndPersistsEmptyArray(t *testing.T) {
	f, e, r, source := newCopyExecution(t)
	outside := t.TempDir()
	gate2Write(t, filepath.Join(outside, "file"), "outside worktree dummy")
	for name, target := range map[string]string{
		"file-link": "source", "outside-file": filepath.Join(outside, "file"),
		"dir-link": ".", "outside-dir": outside,
	} {
		if err := os.Symlink(target, filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
		e.Claim.Project.CopyAllowlist = append(e.Claim.Project.CopyAllowlist, name)
		if strings.Contains(name, "dir") {
			leaf := "source"
			if name == "outside-dir" {
				leaf = "file"
			}
			e.Claim.Project.CopyAllowlist = append(e.Claim.Project.CopyAllowlist, name+"/"+leaf)
		}
	}
	if err := e.CopyWorktreeFiles(context.Background(), r, store.WorktreeCopyStore{DB: f.db}); err != nil {
		t.Fatal(err)
	}
	var saved string
	if err := f.db.QueryRow(`SELECT copied_files FROM worktrees WHERE id=?`, e.WorktreeID).Scan(&saved); err != nil || saved != "[]" {
		t.Fatalf("empty manifest=%s err=%v", saved, err)
	}
	for _, name := range e.Claim.Project.CopyAllowlist {
		if _, err := os.Lstat(filepath.Join(e.WorktreePath, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink copied: %s %v", name, err)
		}
	}
}

func TestCopyWorktreeFilesReplacesTrackedFileAndPersistsSHA256(t *testing.T) {
	f, e, r, source := newCopyExecution(t)
	// "source" is tracked in the fetched Base Branch and in the owner checkout.
	path := filepath.Join(e.WorktreePath, "source")
	if body, err := os.ReadFile(path); err != nil || string(body) != "initial" {
		t.Fatalf("tracked Base Branch file: %q err=%v", body, err)
	}
	gate2Write(t, filepath.Join(source, "source"), "local")
	before := gate2Git(t, source, "status", "--porcelain")
	e.Claim.Project.CopyAllowlist = []string{"source"}
	if err := e.CopyWorktreeFiles(context.Background(), r, store.WorktreeCopyStore{DB: f.db}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "local" {
		t.Fatalf("tracked destination not replaced: %q err=%v", body, err)
	}
	var manifest, status string
	if err := f.db.QueryRow(`SELECT w.copied_files,e.status FROM worktrees w
		JOIN executions e ON e.worktree_id=w.id WHERE e.id=? AND w.id=?`, logExecution, e.WorktreeID).
		Scan(&manifest, &status); err != nil || status != "preparing" {
		t.Fatalf("manifest=%s status=%s err=%v", manifest, status, err)
	}
	var saved []model.CopiedFile
	if err := json.Unmarshal([]byte(manifest), &saved); err != nil {
		t.Fatal(err)
	}
	want := []model.CopiedFile{{Path: "source", SHA256: fmt.Sprintf("%x", sha256.Sum256(body))}}
	if !reflect.DeepEqual(saved, want) || !reflect.DeepEqual(e.CopiedFiles, want) {
		t.Fatalf("saved=%+v execution=%+v want=%+v", saved, e.CopiedFiles, want)
	}
	if body, err := os.ReadFile(filepath.Join(source, "source")); err != nil || string(body) != "local" ||
		before != gate2Git(t, source, "status", "--porcelain") {
		t.Fatalf("owner checkout changed: %q err=%v", body, err)
	}
	if _, clean, err := gate.CheckWorktree(context.Background(), e.WorktreePath, saved); err != nil || !clean {
		t.Fatalf("tracked copy exemption: clean=%v err=%v", clean, err)
	}
}

func TestCopyWorktreeFilesRecordingFailureRollsBackAssociation(t *testing.T) {
	f, e, r, source := newCopyExecution(t)
	gate2Write(t, filepath.Join(source, "dummy"), "dummy")
	e.Claim.Project.CopyAllowlist = []string{"dummy"}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_worktree_association BEFORE UPDATE OF worktree_id ON executions
		BEGIN SELECT RAISE(ABORT, 'reject association'); END`); err != nil {
		t.Fatal(err)
	}
	err := e.CopyWorktreeFiles(context.Background(), r, store.WorktreeCopyStore{DB: f.db})
	if err == nil || e.WorktreeID != "" {
		t.Fatalf("record failure err=%v id=%s", err, e.WorktreeID)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM worktrees`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial worktree row count=%d err=%v", count, err)
	}
	if body, err := os.ReadFile(filepath.Join(e.WorktreePath, "dummy")); err != nil || string(body) != "dummy" {
		t.Fatalf("failed persistence removed retained copy: %q %v", body, err)
	}
}

type copyFilesFunc func(context.Context, string, string, []string, func(context.Context, string) error) ([]model.CopiedFile, error)

func (f copyFilesFunc) CopyFiles(ctx context.Context, src, dst string, paths []string, warn func(context.Context, string) error) ([]model.CopiedFile, error) {
	return f(ctx, src, dst, paths, warn)
}

type copyRecorderStub struct{ calls int }

func (s *copyRecorderStub) RecordCopiedWorktree(context.Context, string, model.PreparedWorktree) (string, error) {
	s.calls++
	return "recorded", nil
}

func (*copyRecorderStub) WarnMissingCopy(context.Context, string, string) error { return nil }

func TestCopyWorktreeFilesGuardsAndRedactsErrors(t *testing.T) {
	for _, name := range []string{"reuse", "missing-path", "already-recorded", "cancelled", "copy-failure"} {
		t.Run(name, func(t *testing.T) {
			control := testExecutionControl()
			reports := 0
			control.Reporter = reportFunc(func(_ context.Context, _ string, result ExecutionResult) error {
				reports++
				if result.Status != model.ExecutionFailed || result.ErrorClass != model.ErrorConfig || strings.Contains(result.ErrorMessage, "secret") {
					t.Fatalf("unsafe report: %+v", result)
				}
				return nil
			})
			e := &Execution{control: &control, status: model.ExecutionPreparing, WorktreePath: "/worktree", BaseSHA: strings.Repeat("a", 40), BranchName: "evaloom/task-001-test"}
			e.Claim.Execution.Mode = "continue"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "reuse":
				e.Claim.Worktree = &ClaimWorktree{}
			case "missing-path":
				e.WorktreePath = ""
			case "already-recorded":
				e.WorktreeID = "previous"
			case "cancelled":
				cancel()
			}
			recorder, copies := &copyRecorderStub{}, 0
			err := e.CopyWorktreeFiles(ctx, copyFilesFunc(func(context.Context, string, string, []string, func(context.Context, string) error) ([]model.CopiedFile, error) {
				copies++
				return nil, errors.New("credential=secret")
			}), recorder)
			if err == nil || strings.Contains(err.Error(), "secret") || recorder.calls != 0 {
				t.Fatalf("err=%v saves=%d", err, recorder.calls)
			}
			if name == "copy-failure" {
				if copies != 1 || reports != 1 {
					t.Fatalf("copies=%d reports=%d", copies, reports)
				}
			} else if copies != 0 || reports != 0 {
				t.Fatalf("guard side effects: copies=%d reports=%d", copies, reports)
			}
		})
	}
}
