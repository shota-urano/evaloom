package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRemoveRejectsOwnerRootStorageAndUnregisteredDirectory(t *testing.T) {
	f, r, repository, create := prepareCreation(t)
	created, err := r.Create(context.Background(), create)
	if err != nil {
		t.Fatal(err)
	}
	ownerBefore := snapshot(t, f.owner)
	root, err := filepath.EvalSymlinks(r.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(root, "task-999")
	if err := os.Mkdir(unknown, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.owner, root, filepath.Join(root, repositoryDirectory), repository, unknown} {
		if err := r.Remove(context.Background(), RemoveRequest{ProjectID: create.ProjectID, RepositoryURL: create.RepositoryURL,
			Path: path, BranchName: created.BranchName}); !errors.Is(err, ErrWorktreePath) {
			t.Fatalf("unsafe cleanup path %s: %v", path, err)
		}
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("unsafe cleanup removed path %s: %v", path, err)
		}
	}
	if !reflect.DeepEqual(ownerBefore, snapshot(t, f.owner)) {
		t.Fatal("cleanup touched owner repository")
	}
	if got := git(t, repository, "rev-parse", "refs/heads/"+created.BranchName); got != create.BaseSHA {
		t.Fatal("cleanup changed branch")
	}
}

func TestRemoveMissingDirectoryClearsOnlyItsRegistrationAndKeepsPackedBranch(t *testing.T) {
	_, r, repository, create := prepareCreation(t)
	ctx := context.Background()
	created, err := r.Create(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	create.FreshNumber = 1
	other, err := r.Create(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	harnessGit(t, repository, "pack-refs", "--all")
	// Simulate manual directory loss. Registration still exists in Git.
	if _, err := os.ReadFile(filepath.Join(created.Path, "file.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(created.Path); err != nil {
		t.Fatal(err)
	}
	request := RemoveRequest{ProjectID: create.ProjectID, RepositoryURL: create.RepositoryURL, Path: created.Path, BranchName: created.BranchName}
	for i := 0; i < 2; i++ {
		if err := r.Remove(ctx, request); err != nil {
			t.Fatal(err)
		}
		if got := git(t, repository, "rev-parse", "--verify", "refs/heads/"+created.BranchName); got != create.BaseSHA {
			t.Fatal("cleanup lost packed branch")
		}
	}
	if _, err := os.Stat(other.Path); err != nil || git(t, other.Path, "symbolic-ref", "HEAD") != "refs/heads/"+other.BranchName {
		t.Fatalf("cleanup changed another worktree: %v", err)
	}
	// Existing reuse must still recognize the remaining branch.
	if restored, err := r.Reuse(ctx, ReuseRequest{ProjectID: create.ProjectID, RepositoryURL: create.RepositoryURL,
		Path: created.Path, BranchName: created.BranchName, BaseSHA: create.BaseSHA, ExpectedHeadSHA: create.BaseSHA,
		BeforeRecreate: func(context.Context) error { return nil }}); err != nil || !restored.Recreated {
		t.Fatalf("cleanup branch cannot be reused: %+v %v", restored, err)
	}
}

func TestRemoveCancelledContextDoesNotDeleteWorktree(t *testing.T) {
	_, r, _, create := prepareCreation(t)
	created, err := r.Create(context.Background(), create)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Remove(ctx, RemoveRequest{ProjectID: create.ProjectID, RepositoryURL: create.RepositoryURL,
		Path: created.Path, BranchName: created.BranchName}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cleanup: %v", err)
	}
	if _, err := os.Stat(created.Path); err != nil {
		t.Fatal(err)
	}
}
