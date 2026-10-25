package projectcontext_test

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

	"github.com/shota-urano/evaloom/backend/internal/projectcontext"
)

func TestReadEnumeratesProjectInstructions(t *testing.T) {
	repo := newRepository(t)
	want := map[string]string{
		"AGENTS.md":                           "abc",
		"backend/AGENTS.md":                   "backend instructions\n",
		"backend/internal/AGENTS.md":          "nested instructions\r\n",
		"space dir/AGENTS.md":                 "spaces\n",
		"日本語/AGENTS.md":                       "日本語の指示\n",
		"tab\tline\ncolon:quote\"/AGENTS.md":  "unusual path\n",
		"CLAUDE.md":                           "Claude root\n",
		"frontend/CLAUDE.md":                  "Claude nested\n",
		".mcp.json":                           "{}\n",
		"frontend/.mcp.json":                  "{\"mcpServers\":{}}\n",
		".claude/settings.json":               "{}\n",
		".claude/commands/review.md":          "review\n",
		".claude/agents/reviewer.md":          "reviewer\n",
		".claude/skills/check/SKILL.md":       "skill\n",
		".claude/skills/check/scripts/run.sh": "#!/bin/sh\n",
		".codex/config.toml":                  "model = \"example\"\n",
		".codex/skills/check/SKILL.md":        "Codex skill\n",
		"frontend/.claude/settings.json":      "nested Claude settings\n",
		"backend/.codex/config.toml":          "nested Codex settings\n",
	}
	for name, content := range want {
		writeFile(t, repo, name, content)
	}
	for _, name := range []string{
		"README.md", "lowercase/agents.md", "AGENTS.md.bak", "docs/CLAUDE.md.txt",
		".claude-backup/settings.json", "my.codex/config.toml", ".mcp.json.bak",
		"AGENTS.md-directory/notes.md",
	} {
		writeFile(t, repo, name, "not an instruction\n")
	}
	if err := os.Chmod(filepath.Join(repo, ".claude/skills/check/scripts/run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	baseSHA := commit(t, repo)
	files, err := projectcontext.Read(context.Background(), repo, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, files, want, baseSHA)
	for _, file := range files {
		if file.Path == "AGENTS.md" && file.ContentHash != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
			t.Errorf("SHA-256 of abc = %q", file.ContentHash)
		}
	}
}

func TestReadUsesBaseCommitAfterHeadIndexAndWorkingTreeChange(t *testing.T) {
	repo := newRepository(t)
	want := map[string]string{
		"AGENTS.md":             "base root\r\n",
		"backend/AGENTS.md":     "base backend\n",
		"CLAUDE.md":             "base Claude\n",
		".codex/config.toml":    "base Codex\n",
		".claude/settings.json": "base Claude settings\n",
		".mcp.json":             "base MCP\n",
	}
	for name, content := range want {
		writeFile(t, repo, name, content)
	}
	baseSHA := commit(t, repo)
	for name := range want {
		writeFile(t, repo, name, "later commit\n")
	}
	writeFile(t, repo, "new/AGENTS.md", "added after base\n")
	commit(t, repo)

	writeFile(t, repo, "AGENTS.md", "staged instructions\n")
	git(t, repo, "add", "AGENTS.md")
	writeFile(t, repo, "AGENTS.md", "working tree instructions\n")
	writeFile(t, repo, "untracked/AGENTS.md", "untracked instructions\n")
	if err := os.Remove(filepath.Join(repo, "backend/AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	before := git(t, repo, "status", "--porcelain=v1", "-z")
	files, err := projectcontext.Read(context.Background(), repo, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, files, want, baseSHA)
	if after := git(t, repo, "status", "--porcelain=v1", "-z"); after != before {
		t.Fatal("Read changed the index or working tree")
	}
}

func TestReadNoInstructions(t *testing.T) {
	repo := newRepository(t)
	writeFile(t, repo, "README.md", "ordinary repository\n")
	baseSHA := commit(t, repo)
	files, err := projectcontext.Read(context.Background(), repo, baseSHA)
	if err != nil || len(files) != 0 {
		t.Fatalf("Read = (%v, %v), want empty snapshot without error", files, err)
	}
}

func TestReadRejectsInvalidBaseCommit(t *testing.T) {
	repo := newRepository(t)
	writeFile(t, repo, "AGENTS.md", "instructions\n")
	commit(t, repo)
	for _, revision := range []string{
		"", "HEAD", "--help", strings.Repeat("g", 40), strings.Repeat("0", 40),
		strings.TrimSpace(git(t, repo, "rev-parse", "HEAD^{tree}")),
	} {
		t.Run(revision, func(t *testing.T) {
			files, err := projectcontext.Read(context.Background(), repo, revision)
			if err == nil || files != nil {
				t.Fatalf("Read(%q) = (%v, %v), want nil snapshot and error", revision, files, err)
			}
		})
	}
}

func TestReadRejectsInstructionSymlink(t *testing.T) {
	repo := newRepository(t)
	writeFile(t, repo, "AGENTS.md", "root\n")
	writeFile(t, repo, "real.md", "target instructions\n")
	if err := os.Symlink("real.md", filepath.Join(repo, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	baseSHA := commit(t, repo)
	files, err := projectcontext.Read(context.Background(), repo, baseSHA)
	if err == nil || files != nil || !strings.Contains(err.Error(), "CLAUDE.md") {
		t.Fatalf("Read = (%v, %v), want no partial snapshot and error identifying symlink", files, err)
	}
}

func TestReadCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	files, err := projectcontext.Read(ctx, t.TempDir(), strings.Repeat("a", 40))
	if files != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Read = (%v, %v), want nil snapshot and context.Canceled", files, err)
	}
}

func assertSnapshot(t *testing.T, files []projectcontext.File, want map[string]string, baseSHA string) {
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
		if !exists {
			t.Errorf("unexpected instruction %q", file.Path)
			continue
		}
		if file.Content != content {
			t.Errorf("%q content = %q, want %q", file.Path, file.Content, content)
		}
		hash := sha256.Sum256([]byte(content))
		if file.ContentHash != hex.EncodeToString(hash[:]) {
			t.Errorf("%q hash = %q, want SHA-256 of base content", file.Path, file.ContentHash)
		}
		if file.SourceCommitSHA != baseSHA {
			t.Errorf("%q source commit = %q, want %q", file.Path, file.SourceCommitSHA, baseSHA)
		}
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Errorf("instruction paths = %q, want %q", gotPaths, wantPaths)
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
	cmd.Env = append(os.Environ(),
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
