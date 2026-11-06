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
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// This host launcher is test-only. Production uses codex.ContainerLaunch. The
// fake codex executable commits in a real linked worktree; the production
// Runner collects the resulting Git data and persists the diff via its API.
func TestCodexAdapterCommitReachesRunnerHarnessResult(t *testing.T) {
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
	gate2Git(t, repo, "worktree", "add", "--quiet", "-b", "codex", worktree, base)
	settings, err := harnessenv.Create(context.Background(), harnessenv.Config{Root: t.TempDir(), CommitSHA: base, ConfigVariable: "CODEX_HOME", Files: []gitcontext.File{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := settings.Close(); err != nil {
			t.Error(err)
		}
	})
	bin := t.TempDir()
	fake := filepath.Join(bin, "codex")
	script := `#!/bin/sh
set -eu
test -n "$HOME"
test -n "$CODEX_HOME"
test -z "${RUNNER_TOKEN:-}"
if [ "$1" = login ]; then
  test "$2" = status
  exit 0
fi
test "$1" = exec
shift
test "$1" = --json
printf 'committed by fake codex\n' > tracked.txt
git add tracked.txt
git commit --quiet -m 'Codex change'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"implemented task"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":2}}'
`
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	launch := func(ctx context.Context, env adapter.SandboxEnv, args []string, out, errOut io.Writer) (sandbox.Process, error) {
		command := args[0]
		if command == "codex" {
			command = fake
		}
		cmd := exec.CommandContext(ctx, command, args[1:]...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = env.Worktree, out, errOut
		cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + env.ConfigDir,
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return &codexTestProcess{cmd: cmd}, nil
	}
	store := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(store, []byte(`{"dummy":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	var registry adapter.Registry
	if err := registry.Register("codex", codex.New(codex.Config{ResolveAuthentication: func(adapter.HarnessRequest) (string, error) { return store, nil }, Launch: launch, CancelGrace: time.Second,
		ResolveConfig: func(adapter.SandboxEnv) (*harnessenv.Environment, error) { return settings, nil },
		ResolveToolPermissions: func(context.Context, adapter.HarnessRequest) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		}})); err != nil {
		t.Fatal(err)
	}
	h, _ := registry.Lookup("codex")
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
	if !strings.Contains(string(diff), "committed by fake codex") {
		t.Fatal(string(diff))
	}
	assertHarnessDiff(t, f, e, diff)
}

type codexTestProcess struct {
	cmd  *exec.Cmd
	once sync.Once
	err  error
}

func (p *codexTestProcess) Wait() error           { p.once.Do(func() { p.err = p.cmd.Wait() }); return p.err }
func (p *codexTestProcess) Stop() error           { return p.cmd.Process.Signal(os.Interrupt) }
func (p *codexTestProcess) Kill() error           { return p.cmd.Process.Kill() }
func (p *codexTestProcess) ExitCode() (int, bool) { return p.cmd.ProcessState.ExitCode(), true }
