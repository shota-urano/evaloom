package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedOutput struct {
	mu   sync.Mutex
	text strings.Builder
}

func (o *lockedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.Write(p)
}

func (o *lockedOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}

func evaloomGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", repo}, args...)...)
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Spec 07 3.5 (Q206): evaloomd pins the -evaloom-repo HEAD commit at startup.
func TestRunPinsEvaloomRepositoryHeadAtStartup(t *testing.T) {
	repo := t.TempDir()
	evaloomGit(t, repo, "init", "--quiet", "--initial-branch=main")
	if err := os.MkdirAll(filepath.Join(repo, "context", "global"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "context", "global", "policy.md"), []byte("policy"), 0o644); err != nil {
		t.Fatal(err)
	}
	evaloomGit(t, repo, "add", "--all")
	evaloomGit(t, repo, "commit", "--quiet", "-m", "global")
	head := evaloomGit(t, repo, "rev-parse", "HEAD")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output := &lockedOutput{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-db", filepath.Join(t.TempDir(), "evaloom.db"), "-port", "0", "-evaloom-repo", repo}, output)
	}()
	for !strings.Contains(output.String(), "evaloomd: listening on ") {
		select {
		case err := <-done:
			t.Fatalf("server exited before startup: %v\n%s", err, output)
		case <-ctx.Done():
			t.Fatalf("server did not start:\n%s", output)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(output.String(), "Global Context pinned") || !strings.Contains(output.String(), "commit="+head) {
		t.Fatalf("startup did not pin HEAD %s:\n%s", head, output)
	}
}

// A path that is not a Git repository root, or has no commit yet, cannot supply a
// pinned revision; evaloomd refuses to start instead of reading the working tree.
func TestRunRejectsEvaloomRepositoryWithoutHeadCommit(t *testing.T) {
	unborn := t.TempDir()
	evaloomGit(t, unborn, "init", "--quiet", "--initial-branch=main")
	parent := t.TempDir()
	evaloomGit(t, parent, "init", "--quiet", "--initial-branch=main")
	evaloomGit(t, parent, "commit", "--quiet", "--allow-empty", "-m", "initial")
	subdirectory := filepath.Join(parent, "context")
	if err := os.Mkdir(subdirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, repo := range map[string]string{"plain directory": t.TempDir(), "no commit": unborn, "missing": filepath.Join(t.TempDir(), "missing"),
		"subdirectory": subdirectory, "relative": "relative/repo"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evaloom.db")
			err := run(context.Background(), []string{"-db", path, "-port", "0", "-evaloom-repo", repo}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "evaloom repository") {
				t.Fatalf("run: %v; want evaloom repository error", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid configuration created database: %v", err)
			}
		})
	}
}
