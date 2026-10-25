package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/registry"
)

func TestValidateRepositoryRejectsFailedPreconditions(t *testing.T) {
	input := newRepository(t)
	nonRepo := t.TempDir()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(input.Path, "subdir")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*registry.RepositoryInput)
		field  string
		reason string
	}{
		{"relative path", func(i *registry.RepositoryInput) { i.Path = "repo" }, "path", "not_absolute"},
		{"missing path", func(i *registry.RepositoryInput) { i.Path = filepath.Join(nonRepo, "missing") }, "path", "not_found"},
		{"file", func(i *registry.RepositoryInput) { i.Path = file }, "path", "not_directory"},
		{"not Git", func(i *registry.RepositoryInput) { i.Path = nonRepo }, "path", "not_git_repository"},
		{"subdirectory", func(i *registry.RepositoryInput) { i.Path = subdir }, "path", "not_git_repository"},
		{"different URL", func(i *registry.RepositoryInput) { i.URL = "https://user:secret@example.invalid/other.git" }, "url", "remote_mismatch"},
		{"empty URL", func(i *registry.RepositoryInput) { i.URL = "" }, "url", "remote_mismatch"},
		{"missing remote", func(i *registry.RepositoryInput) { i.Remote = "missing" }, "url", "remote_not_found"},
		{"missing branch", func(i *registry.RepositoryInput) { i.BaseBranch = "missing" }, "baseBranch", "not_found"},
		{"invalid branch", func(i *registry.RepositoryInput) { i.BaseBranch = "main*" }, "baseBranch", "invalid_branch"},
		{"empty worktree root", func(i *registry.RepositoryInput) { i.WorktreeRoot = "" }, "worktreeRoot", "not_absolute"},
		{"file worktree root", func(i *registry.RepositoryInput) { i.WorktreeRoot = file }, "worktreeRoot", "not_resolvable"},
		{"inside worktree root", func(i *registry.RepositoryInput) { i.WorktreeRoot = filepath.Dir(i.Path) }, "path", "within_worktree_root"},
		{"equal worktree root", func(i *registry.RepositoryInput) { i.WorktreeRoot = i.Path }, "path", "within_worktree_root"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := input
			test.change(&candidate)
			assertRejected(t, candidate, test.field, test.reason)
		})
	}
}

func TestValidateRepositoryReturnsRealPathWithoutChangingRepository(t *testing.T) {
	input := newRepository(t)
	real := realPath(t, input.Path)
	link := filepath.Join(t.TempDir(), "repository link")
	if err := os.Symlink(input.Path, link); err != nil {
		t.Fatal(err)
	}
	input.Path = link
	before := repositoryState(t, real)
	got, err := registry.ValidateRepository(context.Background(), input)
	if err != nil || got != real {
		t.Fatalf("ValidateRepository = (%q, %v), want real path %q", got, err, real)
	}
	if after := repositoryState(t, real); after != before {
		t.Fatal("validation changed the repository's working tree, refs, or config")
	}
}

func TestValidateRepositoryUsesRealPathsForWorktreeCollision(t *testing.T) {
	input := newRepository(t)
	link := filepath.Join(t.TempDir(), "root link")
	if err := os.Symlink(filepath.Dir(input.Path), link); err != nil {
		t.Fatal(err)
	}
	input.WorktreeRoot = link
	assertRejected(t, input, "path", "within_worktree_root")

	input.WorktreeRoot = filepath.Dir(input.Path)
	input.Path = filepath.Join(link, filepath.Base(input.Path))
	assertRejected(t, input, "path", "within_worktree_root")
}

func TestValidateRepositoryRejectsWorktreeCollisionWithDifferentCase(t *testing.T) {
	input := newRepository(t)
	for _, test := range []struct {
		name string
		root string
	}{
		{"ancestor", filepath.Dir(input.Path)},
		{"same directory", input.Path},
	} {
		t.Run(test.name, func(t *testing.T) {
			alias := strings.ToUpper(test.root)
			originalInfo, err := os.Stat(test.root)
			if err != nil {
				t.Fatal(err)
			}
			aliasInfo, err := os.Stat(alias)
			if errors.Is(err, os.ErrNotExist) {
				t.Skip("filesystem does not resolve the differently cased directory")
			}
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(originalInfo, aliasInfo) {
				t.Skip("differently cased paths identify distinct directories")
			}
			candidate := input
			candidate.WorktreeRoot = alias
			assertRejected(t, candidate, "path", "within_worktree_root")
		})
	}
}

func TestValidateRepositoryAllowsSiblingWithWorktreeRootPrefix(t *testing.T) {
	input := newRepository(t)
	// A string prefix check would incorrectly reject this sibling.
	input.WorktreeRoot = strings.TrimSuffix(input.Path, "repository") + "repo"
	if err := os.Mkdir(input.WorktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	assertValid(t, input)
}

func TestValidateRepositoryAllowsWorktreeRootNotYetCreated(t *testing.T) {
	input := newRepository(t)
	input.WorktreeRoot = filepath.Join(t.TempDir(), "future", "worktrees")
	assertValid(t, input)
	if _, err := os.Lstat(input.WorktreeRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validator created worktree root or stat failed unexpectedly: %v", err)
	}
}

func TestValidateRepositoryRejectsDanglingSymlinks(t *testing.T) {
	input := newRepository(t)
	link := filepath.Join(t.TempDir(), "dangling")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), link); err != nil {
		t.Fatal(err)
	}
	input.WorktreeRoot = filepath.Join(link, "future")
	assertRejected(t, input, "worktreeRoot", "not_resolvable")
	input.WorktreeRoot = t.TempDir()
	input.Path = link
	assertRejected(t, input, "path", "not_found")
}

func TestValidateRepositoryRejectsUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read directories regardless of mode bits")
	}
	input := newRepository(t)
	if err := os.Chmod(input.Path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(input.Path, 0o700); err != nil {
			t.Error(err)
		}
	})
	assertRejected(t, input, "path", "not_readable")
}

func TestValidateRepositoryChecksLiveRemoteBranch(t *testing.T) {
	input := newRepository(t)
	sha := strings.TrimSpace(runGit(t, input.URL, "rev-parse", "refs/heads/main"))
	// Leave the cached origin/main in place after deleting the actual remote ref.
	runGit(t, input.URL, "update-ref", "-d", "refs/heads/main")
	assertRejected(t, input, "baseBranch", "not_found")
	// A tag with the same name is not a Base Branch.
	runGit(t, input.URL, "update-ref", "refs/tags/main", sha)
	assertRejected(t, input, "baseBranch", "not_found")
}

func TestValidateRepositoryFindsBranchNotFetchedLocally(t *testing.T) {
	input := newRepository(t)
	runGit(t, input.URL, "update-ref", "refs/heads/new-branch", "refs/heads/main")
	input.BaseBranch = "new-branch"
	assertValid(t, input)
}

func TestValidateRepositoryFailsClosedWhenRemoteUnavailable(t *testing.T) {
	input := newRepository(t)
	input.URL = filepath.Join(t.TempDir(), "unavailable.git")
	runGit(t, input.Path, "remote", "set-url", "origin", input.URL)
	assertRejected(t, input, "baseBranch", "remote_unavailable")
}

func TestValidateRepositorySupportsConfiguredRemoteAndHarness(t *testing.T) {
	input := newRepository(t)
	runGit(t, input.Path, "remote", "rename", "origin", "upstream")
	input.Remote = "upstream"
	assertValid(t, input)
	// A Harness has no Base Branch, so no remote connection is required.
	input.BaseBranch = ""
	input.URL = filepath.Join(t.TempDir(), "offline.git")
	runGit(t, input.Path, "remote", "set-url", "upstream", input.URL)
	assertValid(t, input)
}

func TestValidateRepositoryRejectsBareRepositoryButAcceptsLinkedWorktree(t *testing.T) {
	input := newRepository(t)
	bare := input
	bare.Path = input.URL
	assertRejected(t, bare, "path", "not_git_repository")
	linked := filepath.Join(t.TempDir(), "linked repository\n")
	runGit(t, input.Path, "worktree", "add", "--quiet", "--detach", linked, "HEAD")
	input.Path = linked
	assertValid(t, input)
}

func TestValidateRepositoryIgnoresAmbientRepositoryOverrides(t *testing.T) {
	input := newRepository(t)
	t.Setenv("GIT_DIR", filepath.Join(input.Path, ".git"))
	t.Setenv("GIT_WORK_TREE", input.Path)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "remote.origin.url")
	t.Setenv("GIT_CONFIG_VALUE_0", "ambient override")
	assertValid(t, input)
	input.Path = t.TempDir()
	assertRejected(t, input, "path", "not_git_repository")
}

func TestValidateRepositoryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := registry.ValidateRepository(ctx, registry.RepositoryInput{})
	if got != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("ValidateRepository = (%q, %v), want empty path and context.Canceled", got, err)
	}
}

func assertRejected(t *testing.T, input registry.RepositoryInput, field, reason string) {
	t.Helper()
	got, err := registry.ValidateRepository(context.Background(), input)
	var failure *registry.PreconditionError
	if got != "" || !errors.As(err, &failure) {
		t.Fatalf("ValidateRepository = (%q, %v), want empty path and PreconditionError", got, err)
	}
	if failure.StatusCode() != 422 || failure.Code != "precondition_failed" {
		t.Fatalf("error = %+v, want 422 precondition_failed", failure)
	}
	if len(failure.Details) != 1 || failure.Details[0] != (registry.Detail{Field: field, Reason: reason}) {
		t.Errorf("details = %+v, want field=%s reason=%s", failure.Details, field, reason)
	}
	encoded, marshalErr := json.Marshal(failure)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), "secret") || strings.Contains(err.Error(), "secret") {
		t.Fatal("validation error exposed remote URL credentials")
	}
}

func assertValid(t *testing.T, input registry.RepositoryInput) {
	t.Helper()
	got, err := registry.ValidateRepository(context.Background(), input)
	if want := realPath(t, input.Path); err != nil || got != want {
		t.Fatalf("ValidateRepository = (%q, %v), want (%q, nil)", got, err, want)
	}
}

func newRepository(t *testing.T) registry.RepositoryInput {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repository")
	remote := filepath.Join(root, "remote.git")
	runGit(t, root, "init", "--quiet", "--initial-branch=main", repo)
	runGit(t, root, "init", "--quiet", "--bare", "--initial-branch=main", remote)
	runGit(t, repo, "commit", "--quiet", "--allow-empty", "-m", "initial")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "--quiet", "origin", "main")
	return registry.RepositoryInput{Path: repo, URL: remote, BaseBranch: "main", WorktreeRoot: t.TempDir()}
}

func repositoryState(t *testing.T, repo string) string {
	t.Helper()
	return runGit(t, repo, "status", "--porcelain=v1", "-z") +
		runGit(t, repo, "show-ref") + runGit(t, repo, "config", "--local", "--list")
}

func realPath(t *testing.T, name string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(name)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func runGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", repo}, args...)...)
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
