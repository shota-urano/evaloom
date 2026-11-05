package runner

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/claudecode"
	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// This host launcher is test-only. Production uses claudecode.ContainerLaunch. The
// fake claude executable commits in a real linked worktree; the production
// Runner collects the resulting Git data and persists the diff via its API.
func TestClaudeCodeAdapterCommitReachesRunnerHarnessResult(t *testing.T) {
	f := newLogControlPlane(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	repo, worktree := t.TempDir(), filepath.Join(t.TempDir(), "worktree")
	gate2Git(t, repo, "init", "--quiet")
	gate2Write(t, filepath.Join(repo, "tracked.txt"), "base\n")
	gate2Git(t, repo, "add", ".")
	gate2Git(t, repo, "commit", "--quiet", "-m", "base")
	base := gate2Git(t, repo, "rev-parse", "HEAD")
	gate2Git(t, repo, "worktree", "add", "--quiet", "-b", "claudecode", worktree, base)
	settings, err := harnessenv.Create(context.Background(), harnessenv.Config{Root: t.TempDir(), CommitSHA: base, ConfigVariable: "CLAUDE_CONFIG_DIR", Files: []gitcontext.File{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := settings.Close(); err != nil {
			t.Error(err)
		}
	})
	bin := t.TempDir()
	fake := filepath.Join(bin, "claude")
	script := `#!/bin/sh
set -eu
test -n "$HOME"
test -n "$CLAUDE_CONFIG_DIR"
test -z "${RUNNER_TOKEN:-}"
test "$1" = --setting-sources
test "$2" = user,project
test "$3" = --settings
test "$4" = '{}'
shift 4
if [ "$1" = auth ]; then
  test "$2" = status
  test "$3" = --json
  exit 0
fi
test "$1" = -p
test "$2" = --output-format
test "$3" = stream-json
printf 'committed by fake claudecode\n' > tracked.txt
git add tracked.txt
git commit --quiet -m 'ClaudeCode change'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"implemented task","usage":{"input_tokens":3,"output_tokens":2}}'
`
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	launch := func(ctx context.Context, env adapter.SandboxEnv, args []string, out, errOut io.Writer) (sandbox.Process, error) {
		command := args[0]
		if command == "claude" {
			command = fake
		}
		cmd := exec.CommandContext(ctx, command, args[1:]...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = env.Worktree, out, errOut
		cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + home, "CLAUDE_CONFIG_DIR=" + env.ConfigDir,
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return &claudecodeTestProcess{cmd: cmd}, nil
	}
	store := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(store, []byte(`{"dummy":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	var registry adapter.Registry
	if err := registry.Register("claude_code", claudecode.New(claudecode.Config{ResolveConfig: func(adapter.SandboxEnv) (*harnessenv.Environment, error) { return settings, nil }, ResolveAuthentication: func(adapter.HarnessRequest) (string, error) { return store, nil }, Launch: launch, CancelGrace: time.Second, Configure: func(context.Context, adapter.HarnessRequest) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}})); err != nil {
		t.Fatal(err)
	}
	h, _ := registry.Lookup("claude_code")
	request := adapter.HarnessRequest{ContractVersion: adapter.ContractVersion, ExecutionID: logExecution, Task: adapter.RequestTask{AcceptanceCriteria: "commit change"},
		Project: adapter.RequestProject{Workdir: worktree, BaseSHA: base}, Profile: adapter.RequestProfile{ModelID: "test-model", Definition: json.RawMessage(`{}`)}, Context: adapter.RequestContext{RevisionIDs: []string{}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := h.Start(ctx, request, adapter.SandboxEnv{Worktree: worktree, ConfigDir: settings.ConfigDir(), Env: settings.Env()})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := h.Logs(id)
	if err != nil {
		t.Fatal(err)
	}
	for range lines {
	}
	result, err := h.Result(id)
	if err != nil || result.Status != adapter.ResultSucceeded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	e := &Execution{WorktreePath: worktree, BaseSHA: base, HarnessResult: &result}
	e.Claim.Execution.ID = logExecution
	if err := r.HarnessResultHandler()(ctx, e); err != nil {
		t.Fatal(err)
	}
	head := gate2Git(t, worktree, "rev-parse", "HEAD")
	if result.HeadSHA != head || len(result.Commits) != 1 || result.Commits[0] != head || result.Summary != "implemented task" {
		t.Fatalf("result=%+v", result)
	}
	diff, err := exec.Command("git", "-C", worktree, "diff", base+".."+head, "--").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diff), "committed by fake claudecode") {
		t.Fatal(string(diff))
	}
	assertHarnessDiff(t, f, e, diff)
}

type claudecodeTestProcess struct {
	cmd  *exec.Cmd
	once sync.Once
	err  error
}

func (p *claudecodeTestProcess) Wait() error {
	p.once.Do(func() { p.err = p.cmd.Wait() })
	return p.err
}
func (p *claudecodeTestProcess) Stop() error           { return p.cmd.Process.Signal(os.Interrupt) }
func (p *claudecodeTestProcess) Kill() error           { return p.cmd.Process.Kill() }
func (p *claudecodeTestProcess) ExitCode() (int, bool) { return p.cmd.ProcessState.ExitCode(), true }
