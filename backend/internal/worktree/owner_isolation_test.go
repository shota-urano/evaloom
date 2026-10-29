package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

type repositoryState struct {
	refs, config string
}

func readRepositoryState(t *testing.T, repository string) repositoryState {
	t.Helper()
	// Include every namespace, packed refs and symbolic ref targets, rather
	// than checking only the owner's currently checked out branch.
	refs, err := safegit.Output(context.Background(), repository, "for-each-ref",
		"--sort=refname", "--format=%(refname) %(objectname) %(symref)")
	if err != nil {
		t.Fatal(err)
	}
	common := git(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	config, err := os.ReadFile(filepath.Join(common, "config"))
	if err != nil {
		t.Fatal(err)
	}
	return repositoryState{refs: string(refs), config: string(config)}
}

// harnessGit deliberately bypasses safegit's command restrictions: a harness
// can edit refs and config in its writable Git area. Only disposable test
// repositories are passed here. Host-side worktree creation still uses safegit.
func harnessGit(t *testing.T, repository string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repository}, args...)...)
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if !strings.HasPrefix(name, "GIT_") && name != "SSH_ASKPASS" {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Harness Test", "GIT_AUTHOR_EMAIL=harness@example.invalid",
		"GIT_COMMITTER_NAME=Harness Test", "GIT_COMMITTER_EMAIL=harness@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("harness Git %v: %v\n%s", args, err, out)
	}
}

func TestHarnessGitMutationsProtectOwnerOnlyWithIndependentClone(t *testing.T) {
	for _, independent := range []bool{true, false} {
		name := "B-direct-owner-worktree"
		if independent {
			name = "A-evaloom-clone"
		}
		t.Run(name, func(t *testing.T) {
			f := newRemote(t)
			base := git(t, f.owner, "rev-parse", "HEAD")
			// Exercise packed and loose refs in multiple namespaces. These are
			// owner-side local refs, not merely copies of remote tracking refs.
			harnessGit(t, f.owner, "branch", "owner-delete", base)
			harnessGit(t, f.owner, "tag", "owner-tag", base)
			harnessGit(t, f.owner, "pack-refs", "--all")
			harnessGit(t, f.owner, "update-ref", "refs/owner/private", base)
			harnessGit(t, f.owner, "symbolic-ref", "refs/owner/current", "refs/heads/main")
			harnessGit(t, f.owner, "config", "--local", "evaloom.owner", "original")
			beforePreparation := readRepositoryState(t, f.owner)
			root := t.TempDir()
			repository := f.owner
			if independent {
				var err error
				repository, err = (Repositories{WorktreeRoot: root}).Prepare(context.Background(), "project-a", f.url)
				if err != nil {
					t.Fatal(err)
				}
				// Matching names in the clone make overwrites and deletions
				// meaningful even though owner-only refs are never cloned.
				harnessGit(t, repository, "branch", "owner-delete", base)
				harnessGit(t, repository, "tag", "owner-tag", base)
				harnessGit(t, repository, "pack-refs", "--all")
				harnessGit(t, repository, "update-ref", "refs/owner/private", base)
				harnessGit(t, repository, "symbolic-ref", "refs/owner/current", "refs/heads/main")
			}
			worktree := filepath.Join(root, "execution")
			git(t, repository, "worktree", "add", "-b", "evaloom/isolation-test", worktree, base)
			common := git(t, worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
			ownerCommon := git(t, f.owner, "rev-parse", "--path-format=absolute", "--git-common-dir")
			if (common == ownerCommon) == independent {
				t.Fatalf("unexpected Git area sharing: worktree=%s, owner=%s", common, ownerCommon)
			}
			ownerBefore := readRepositoryState(t, f.owner)
			if independent && ownerBefore != beforePreparation {
				t.Fatal("clone preparation or worktree creation changed owner refs or config")
			}
			if !independent && ownerBefore.refs == beforePreparation.refs {
				t.Fatal("direct worktree creation did not add a branch to owner refs")
			}

			var commit string
			operations := []struct {
				name string
				run  func(*testing.T)
			}{
				{"commit", func(t *testing.T) {
					write(t, filepath.Join(worktree, "file.txt"), "harness edit\n")
					harnessGit(t, worktree, "add", "file.txt")
					harnessGit(t, worktree, "commit", "-m", "harness commit")
					commit = git(t, worktree, "rev-parse", "HEAD")
					if commit == base {
						t.Fatal("harness commit did not advance HEAD")
					}
				}},
				{"rewrite-main", func(t *testing.T) { harnessGit(t, worktree, "update-ref", "refs/heads/main", commit, base) }},
				{"rewrite-remote-tracking-ref", func(t *testing.T) { harnessGit(t, worktree, "update-ref", "refs/remotes/origin/main", commit, base) }},
				{"rewrite-tag", func(t *testing.T) { harnessGit(t, worktree, "update-ref", "refs/tags/owner-tag", commit, base) }},
				{"rewrite-custom-ref", func(t *testing.T) { harnessGit(t, worktree, "update-ref", "refs/owner/private", commit, base) }},
				{"rewrite-symbolic-ref", func(t *testing.T) {
					harnessGit(t, worktree, "symbolic-ref", "refs/owner/current", "refs/heads/evaloom/isolation-test")
				}},
				{"create-branch", func(t *testing.T) { harnessGit(t, worktree, "branch", "harness-created", commit) }},
				{"delete-existing-branch", func(t *testing.T) { harnessGit(t, worktree, "branch", "-D", "owner-delete") }},
				{"delete-created-branch", func(t *testing.T) { harnessGit(t, worktree, "branch", "-D", "harness-created") }},
				{"delete-tag", func(t *testing.T) { harnessGit(t, worktree, "update-ref", "-d", "refs/tags/owner-tag", commit) }},
				{"rewrite-config", func(t *testing.T) { harnessGit(t, worktree, "config", "--local", "evaloom.owner", "harness-modified") }},
				{"rewrite-origin", func(t *testing.T) {
					harnessGit(t, worktree, "remote", "set-url", "origin", "https://example.invalid/harness.git")
				}},
			}
			for _, operation := range operations {
				t.Run(operation.name, func(t *testing.T) {
					targetBefore := readRepositoryState(t, worktree)
					operation.run(t)
					targetAfter := readRepositoryState(t, worktree)
					if targetAfter == targetBefore {
						t.Fatal("harness operation did not change target refs or config")
					}
					ownerAfter := readRepositoryState(t, f.owner)
					if independent {
						if ownerAfter.refs != ownerBefore.refs {
							t.Fatalf("owner refs changed\nbefore:\n%safter:\n%s", ownerBefore.refs, ownerAfter.refs)
						}
						if ownerAfter.config != ownerBefore.config {
							t.Fatalf("owner config changed\nbefore:\n%s\nafter:\n%s", ownerBefore.config, ownerAfter.config)
						}
					} else {
						// Positive control: identical operations through a direct
						// worktree really modify the owner's shared Git area.
						if ownerAfter != targetAfter || ownerAfter == ownerBefore {
							t.Fatal("direct worktree did not propagate its Git changes to owner")
						}
						ownerBefore = ownerAfter
					}
				})
				if t.Failed() {
					t.FailNow()
				}
			}
		})
	}
}
