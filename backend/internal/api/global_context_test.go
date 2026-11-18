package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func globalRepoFile(t *testing.T, repo, name, content string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func globalRepoCommit(t *testing.T, repo string) string {
	t.Helper()
	projectGit(t, repo, "add", "--all")
	projectGit(t, repo, "commit", "--quiet", "--allow-empty", "-m", "snapshot")
	cmd := exec.Command("git", "-C", repo, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// pinGlobalContext runs the context API with the startup-pinned source and
// returns the saved global rows. The status must be 200: an absent Global
// still lets the work stage proceed (Q207).
func pinGlobalContext(t *testing.T, source GlobalContextSource) (global []savedContext, all []savedContext) {
	t.Helper()
	f, token, id, _ := reportFixture(t)
	input := contextInput(t, f, id)
	startStage(t, f, token, id, "work")
	status, body := requestContext(t, f, token, id, source, input)
	var reply executionContextReply
	if status != 200 || json.Unmarshal(body, &reply) != nil {
		t.Fatalf("context: %d %s", status, body)
	}
	var state string
	if err := f.db.QueryRow(`SELECT status FROM executions WHERE id=?`, id).Scan(&state); err != nil || state != "working" {
		t.Fatalf("execution state=%q err=%v; want working", state, err)
	}
	all = savedContexts(t, f, id)
	for _, pin := range all {
		if pin.Layer == "global" {
			global = append(global, pin)
		}
	}
	return global, all
}

// Spec 07 3.5 (Q206): Global comes from the HEAD commit pinned at startup and
// its SHA is the source_revision. Working tree edits, staged changes, untracked
// files and commits made after startup are never read.
func TestCommitGlobalContextReadsStartupHeadCommitOnly(t *testing.T) {
	repo := t.TempDir()
	projectGit(t, repo, "init", "--quiet", "--initial-branch=main")
	globalRepoFile(t, repo, "context/global/policy.md", "committed policy\r\n")
	globalRepoFile(t, repo, "context/global/nested/a.md", "a")
	globalRepoFile(t, repo, "context/global-other/private.md", "private")
	globalRepoFile(t, repo, "context/project-a/private.md", "private")
	startup := globalRepoCommit(t, repo)

	source, err := NewCommitGlobalContext(t.Context(), repo)
	if err != nil || source.Commit != startup {
		t.Fatalf("source=%+v err=%v; want commit %s", source, err, startup)
	}
	// Uncommitted: modified, staged and untracked files under context/global/.
	globalRepoFile(t, repo, "context/global/policy.md", "uncommitted policy")
	globalRepoFile(t, repo, "context/global/staged.md", "staged")
	projectGit(t, repo, "add", "context/global/staged.md")
	globalRepoFile(t, repo, "context/global/untracked.md", "untracked")
	globalRepoFile(t, repo, "context/global/nested/a.md", "modified a")
	// A commit after startup must not move the pinned revision either.
	globalRepoFile(t, repo, "context/global/later.md", "later")
	later := globalRepoCommit(t, repo)
	if later == startup {
		t.Fatal("later commit did not move HEAD")
	}

	global, _ := pinGlobalContext(t, source)
	want := map[string]string{"context/global/nested/a.md": "a", "context/global/policy.md": "committed policy\r\n"}
	if len(global) != len(want) {
		t.Fatalf("global rows=%+v; want %v", global, want)
	}
	for _, pin := range global {
		content, ok := want[pin.Path]
		if !ok || pin.Kind != "global_policy" || pin.Revision != startup || !pin.Content.Valid || pin.Content.String != content || pin.Hash != contextHash([]byte(content)) {
			t.Fatalf("global row %+v; want content %q at %s", pin, content, startup)
		}
	}
}

// Q207: an absent or empty context/global/ creates no global rows and the
// context API still answers so the work stage proceeds. Git keeps no empty
// directory, so "empty" is a working tree directory with uncommitted files.
func TestCommitGlobalContextAbsentOrEmptyCreatesNoGlobalRows(t *testing.T) {
	for _, name := range []string{"absent", "empty"} {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			projectGit(t, repo, "init", "--quiet", "--initial-branch=main")
			globalRepoFile(t, repo, "README.md", "evaloom")
			globalRepoFile(t, repo, "context/global-other/private.md", "private")
			globalRepoCommit(t, repo)
			if name == "empty" {
				if err := os.MkdirAll(filepath.Join(repo, "context", "global"), 0o755); err != nil {
					t.Fatal(err)
				}
				globalRepoFile(t, repo, "context/global/untracked.md", "untracked")
			}
			source, err := NewCommitGlobalContext(t.Context(), repo)
			if err != nil {
				t.Fatal(err)
			}
			files, err := source.ReadGlobalContext(t.Context())
			if err != nil || len(files) != 0 {
				t.Fatalf("files=%+v err=%v; want none", files, err)
			}
			global, all := pinGlobalContext(t, source)
			if len(global) != 0 {
				t.Fatalf("global rows=%+v; want none", global)
			}
			layers := map[string]int{}
			for _, pin := range all {
				layers[pin.Layer]++
			}
			if layers["task"] != 1 || layers["harness"] == 0 {
				t.Fatalf("other layers not pinned: %v", layers)
			}
		})
	}
}

// A linked file in the pinned commit is unreadable Global Context: the
// Execution fails as config_error instead of following the link (spec 07 6章).
func TestCommitGlobalContextRejectsLinkedFile(t *testing.T) {
	repo := t.TempDir()
	projectGit(t, repo, "init", "--quiet", "--initial-branch=main")
	globalRepoFile(t, repo, "private.md", "private")
	if err := os.MkdirAll(filepath.Join(repo, "context", "global"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../private.md", filepath.Join(repo, "context", "global", "link.md")); err != nil {
		t.Fatal(err)
	}
	globalRepoCommit(t, repo)
	source, err := NewCommitGlobalContext(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if files, err := source.ReadGlobalContext(t.Context()); err == nil {
		t.Fatalf("linked Global accepted: %+v", files)
	}
}

func TestNewCommitGlobalContextRequiresCommittedRepository(t *testing.T) {
	plain := t.TempDir()
	if _, err := NewCommitGlobalContext(t.Context(), plain); err == nil {
		t.Fatal("non-repository accepted")
	}
	unborn := t.TempDir()
	projectGit(t, unborn, "init", "--quiet", "--initial-branch=main")
	if _, err := NewCommitGlobalContext(t.Context(), unborn); err == nil {
		t.Fatal("repository without HEAD commit accepted")
	}
	// A subdirectory must not silently resolve its parent repository's HEAD.
	repo := t.TempDir()
	projectGit(t, repo, "init", "--quiet", "--initial-branch=main")
	globalRepoFile(t, repo, "context/global/policy.md", "policy")
	globalRepoCommit(t, repo)
	if _, err := NewCommitGlobalContext(t.Context(), filepath.Join(repo, "context")); err == nil {
		t.Fatal("repository subdirectory accepted")
	}
	if _, err := NewCommitGlobalContext(t.Context(), "relative/repo"); err == nil {
		t.Fatal("relative repository path accepted")
	}
}
