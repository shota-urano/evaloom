package harnesscontext_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/registry"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

const profileYAML = `profileVersion: "1.2.0"
harnessVersion: "1.2.0"
modelId: "test-model"
promptVersion: "first"
skillVersions:
  - { name: "check", version: "first" }
toolConfiguration:
  allowedTools: []
contextPolicyVersion: "1"
executionConfiguration:
  adapter: "codex"
  args: []
collaborationPolicy: "solo"
evaluationPolicyVersion: null
`

func TestReadUsesRegisteredSHAAfterTagHeadIndexAndWorkingTreeChange(t *testing.T) {
	for _, annotated := range []bool{false, true} {
		name := "lightweight tag"
		if annotated {
			name = "annotated tag"
		}
		t.Run(name, func(t *testing.T) {
			repo := newRepository(t)
			want := map[string]string{
				"prompts/developer.md":               "pinned prompt\r\n",
				"skills/check/SKILL.md":              "pinned skill\n",
				"skills/check/scripts/run.sh":        "#!/bin/sh\necho check\n",
				".codex/config.toml":                 "model = \"test-model\"\n",
				"settings/empty.json":                "",
				"space 日本語/tab\tline\ncolon:quote\"": "\x00\xffbinary\r\n",
				"literal[ab]*.md":                    "literal filename\n",
			}
			for name, content := range want {
				writeFile(t, repo, name, content)
			}
			if err := os.Chmod(filepath.Join(repo, "skills/check/scripts/run.sh"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, repo, "evaloom/profile.yaml", profileYAML)
			writeFile(t, repo, "unselected.md", "must not be included\n")
			baseSHA := commit(t, repo)
			tagArgs := []string{"tag"}
			if annotated {
				tagArgs = append(tagArgs, "-a", "-m", "test profile")
			}
			git(t, repo, append(tagArgs, "profile/v1.2.0")...)
			profile, err := registry.ReadProfile(context.Background(), repo, "1.2.0", "codex")
			if err != nil || profile.CommitSHA != baseSHA {
				t.Fatalf("ReadProfile = (%+v, %v), want pin %s", profile, err, baseSHA)
			}

			paths := make([]string, 0, len(want)+1)
			for name := range want {
				paths = append(paths, name)
				writeFile(t, repo, name, "later commit\n")
			}
			writeFile(t, repo, "evaloom/profile.yaml", "invalid replacement definition\n")
			replacement := commit(t, repo)
			git(t, repo, "tag", "-f", "profile/v1.2.0", replacement)
			writeFile(t, repo, "prompts/developer.md", "staged prompt\n")
			git(t, repo, "add", "prompts/developer.md")
			writeFile(t, repo, "prompts/developer.md", "working tree prompt\n")
			writeFile(t, repo, "skills/untracked/SKILL.md", "untracked skill\n")
			if err := os.Remove(filepath.Join(repo, ".codex/config.toml")); err != nil {
				t.Fatal(err)
			}
			// Input order and duplicate selections must not change the snapshot.
			sort.Sort(sort.Reverse(sort.StringSlice(paths)))
			paths = append(paths, "prompts/developer.md")
			before := git(t, repo, "status", "--porcelain=v1", "-z")
			files, err := harnesscontext.Read(context.Background(), repo, profile.CommitSHA, paths)
			if err != nil {
				t.Fatal(err)
			}
			assertSnapshot(t, files, want, baseSHA)
			if after := git(t, repo, "status", "--porcelain=v1", "-z"); after != before {
				t.Fatal("Read changed the index or working tree")
			}
			if got := strings.TrimSpace(git(t, repo, "rev-parse", "refs/tags/profile/v1.2.0")); got != replacement {
				t.Fatalf("tag = %s, want moved tag %s", got, replacement)
			}
		})
	}
}

func TestReadFailuresAreFailedConfigError(t *testing.T) {
	repo := newRepository(t)
	writeFile(t, repo, "a-prompt.md", "abc")
	writeFile(t, repo, "directory/child.md", "child")
	if err := os.Symlink("a-prompt.md", filepath.Join(repo, "link.md")); err != nil {
		t.Fatal(err)
	}
	sha := commit(t, repo)
	git(t, repo, "tag", "-a", "-m", "tag object", "profile/v1.2.0")
	// A gitlink is not a file and its repository must not be traversed.
	git(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+sha+",submodule")
	git(t, repo, "commit", "--quiet", "-m", "gitlink")
	gitlinkSHA := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	for _, tt := range []struct {
		name, sha string
		paths     []string
	}{
		{"nonexistent SHA", strings.Repeat("0", 40), []string{"a-prompt.md"}},
		{"nonexistent SHA with empty selection", strings.Repeat("0", 40), nil},
		{"missing file discards partial result", sha, []string{"a-prompt.md", "missing.md"}},
		{"directory", sha, []string{"directory"}},
		{"symlink", sha, []string{"a-prompt.md", "link.md"}},
		{"submodule", gitlinkSHA, []string{"submodule"}},
		{"submodule contents", gitlinkSHA, []string{"submodule/child.md"}},
		{"empty SHA", "", nil},
		{"moving HEAD", "HEAD", nil},
		{"moving tag", "profile/v1.2.0", nil},
		{"abbreviated SHA", sha[:12], nil},
		{"invalid hex", strings.Repeat("g", 40), nil},
		{"option", "--help", nil},
		{"tree object", strings.TrimSpace(git(t, repo, "rev-parse", sha+"^{tree}")), nil},
		{"blob object", strings.TrimSpace(git(t, repo, "rev-parse", sha+":a-prompt.md")), nil},
		{"tag object", strings.TrimSpace(git(t, repo, "rev-parse", "refs/tags/profile/v1.2.0")), nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files, err := harnesscontext.Read(context.Background(), repo, tt.sha, tt.paths)
			assertConfigFailure(t, files, err)
		})
	}
	// Existence in the working tree cannot satisfy a missing committed file.
	writeFile(t, repo, "missing.md", "uncommitted\n")
	files, err := harnesscontext.Read(context.Background(), repo, sha, []string{"missing.md"})
	assertConfigFailure(t, files, err)
	if !strings.Contains(err.Error(), "missing.md") {
		t.Fatalf("failure does not identify the missing file: %v", err)
	}
	files, err = harnesscontext.Read(context.Background(), t.TempDir(), sha, []string{"a-prompt.md"})
	assertConfigFailure(t, files, err)
}

func TestReadRejectsInvalidPaths(t *testing.T) {
	repo := newRepository(t)
	writeFile(t, repo, "prompt.md", "abc")
	sha := commit(t, repo)
	for _, name := range []string{"", ".", "..", "../prompt.md", "/prompt.md", "dir/../../prompt.md", "./prompt.md", "dir//prompt.md", "dir/../prompt.md", "dir/", "dir\\prompt.md", "prompt\x00.md"} {
		t.Run(name, func(t *testing.T) {
			files, err := harnesscontext.Read(context.Background(), repo, sha, []string{name})
			assertConfigFailure(t, files, err)
		})
	}
}

func TestReadEmptySelection(t *testing.T) {
	repo := newRepository(t)
	writeFile(t, repo, "prompt.md", "abc")
	sha := commit(t, repo)
	files, err := harnesscontext.Read(context.Background(), repo, sha, nil)
	if err != nil || len(files) != 0 {
		t.Fatalf("Read = (%v, %v), want empty snapshot without error", files, err)
	}
}

func TestReadIgnoresReplacementObjectsAndAmbientRepository(t *testing.T) {
	repo := newRepository(t)
	writeFile(t, repo, "prompt.md", "abc")
	sha := commit(t, repo)
	writeFile(t, repo, "prompt.md", "replacement")
	replacement := commit(t, repo)
	git(t, repo, "replace", sha, replacement)
	other := newRepository(t)
	writeFile(t, other, "prompt.md", "ambient repository")
	commit(t, other)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	files, err := harnesscontext.Read(context.Background(), repo, sha, []string{"prompt.md"})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, files, map[string]string{"prompt.md": "abc"}, sha)
	if files[0].ContentHash != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("SHA-256 of abc = %s", files[0].ContentHash)
	}
}

func TestReadPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	files, err := harnesscontext.Read(ctx, t.TempDir(), strings.Repeat("a", 40), nil)
	if files != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Read = (%v, %v), want nil snapshot and context.Canceled", files, err)
	}
}

func assertConfigFailure(t *testing.T, files []harnesscontext.File, err error) {
	t.Helper()
	var failure *harnesscontext.ConfigError
	if files != nil || !errors.As(err, &failure) {
		t.Fatalf("Read = (%v, %v), want nil snapshot and ConfigError", files, err)
	}
	if failure.Status() != model.ExecutionFailed || failure.ErrorClass() != model.ErrorConfig {
		t.Fatalf("failure = %s(%s), want failed(config_error)", failure.Status(), failure.ErrorClass())
	}
	if err := task.ValidateExecutionTransition(model.ExecutionPreparing, failure.Status(), ""); err != nil {
		t.Fatal(err)
	}
	taskStatus, err := task.TaskStatusForExecution(failure.Status())
	if err != nil || taskStatus != model.TaskFailed {
		t.Fatalf("task status = (%s, %v), want failed", taskStatus, err)
	}
}

func assertSnapshot(t *testing.T, files []harnesscontext.File, want map[string]string, sha string) {
	t.Helper()
	wantPaths := make([]string, 0, len(want))
	for name := range want {
		wantPaths = append(wantPaths, name)
	}
	sort.Strings(wantPaths)
	gotPaths := make([]string, 0, len(files))
	for _, file := range files {
		gotPaths = append(gotPaths, file.Path)
		content, exists := want[file.Path]
		hash := sha256.Sum256([]byte(content))
		if !exists || file.Content != content || file.ContentHash != hex.EncodeToString(hash[:]) || file.SourceCommitSHA != sha {
			t.Errorf("unexpected snapshot: %+v; want content %q at %s", file, content, sha)
		}
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Errorf("paths = %q, want %q", gotPaths, wantPaths)
	}
}

func newRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "--quiet")
	return repo
}

func writeFile(t *testing.T, repo, name, content string) {
	t.Helper()
	filePath := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, repo string) string {
	t.Helper()
	git(t, repo, "add", "--all")
	git(t, repo, "commit", "--quiet", "-m", "test snapshot")
	return strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
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
