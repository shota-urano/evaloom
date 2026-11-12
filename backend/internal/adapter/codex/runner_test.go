package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/runner"
)

// Exercise the existing delayed credential-recovery runtime through the stage
// frame too: Result alone cannot detect a later Runner timeout overwrite.
func TestStageFramePreservesResultAfterAuthenticationRecoveryDeadline(t *testing.T) {
	for _, mode := range []string{"writeback-delay-failure", "final-refresh-delay-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, "success")
			useFakeContainerRuntime(t, f, mode)
			f.config.CancelGrace = runner.StageCancelGrace
			f.h = New(f.config)
			f.request.Limits.TimeoutSec = 1
			r, claim := workStageFrame(t, f)
			frame := &finishedWorkFrame{t: t, fixture: f}
			if err := r.ExecuteStages(context.Background(), claim, runner.StageServices{
				Reporter: frame, Executor: frame, Paths: contract.RequestPaths{
					Profile: "/sandbox/profile", Workspace: "/sandbox/workspace", Out: "/sandbox/out",
					Inputs: "/sandbox/inputs", Records: "/sandbox/records", Scratch: "/sandbox/scratch",
				},
			}); err != nil {
				t.Fatal(err)
			}
			if len(frame.outcomes) != 1 || frame.outcomes[0].RunnerError != nil ||
				frame.outcomes[0].Work == nil || frame.outcomes[0].Work.SessionID == nil ||
				*frame.outcomes[0].Work.SessionID == "" || frame.outcomes[0].ExitCode == nil || *frame.outcomes[0].ExitCode != 0 {
				t.Fatalf("credential recovery changed successful stage: %+v", frame.outcomes)
			}
			data, err := os.ReadFile(f.store)
			if err != nil || string(data) != `{"fake":true}` {
				t.Fatalf("failed recovery changed authentication store: %s %v", data, err)
			}
		})
	}
}

type finishedWorkFrame struct {
	t *testing.T
	*fixture
	outcomes []runner.StageOutcome
}

func (f *finishedWorkFrame) StartStage(context.Context, string, model.Stage, []byte) (int, error) {
	return 1, nil
}

func (f *finishedWorkFrame) FinishStage(ctx context.Context, _ string, _ model.Stage, _ int, outcome runner.StageOutcome) (runner.StageNext, error) {
	if ctx.Err() != nil {
		f.t.Fatalf("final report was cancelled: %v", ctx.Err())
	}
	f.outcomes = append(f.outcomes, outcome)
	return runner.StageNext{Next: "stop"}, nil
}

func (f *finishedWorkFrame) RunStage(ctx context.Context, in runner.StageInvocation) runner.StageOutcome {
	if in.Stage != model.StageWork {
		f.t.Fatalf("unexpected stage: %s", in.Stage)
	}
	id, err := f.h.Start(ctx, f.request, f.env)
	if err != nil {
		f.t.Fatal(err)
	}
	result, err := in.WaitWork(ctx, f.h, id)
	if err != nil || result.Status != adapter.ResultSucceeded || result.ErrorClass != nil {
		f.t.Errorf("credential recovery changed finished work: %+v, err=%v", result, err)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		f.t.Fatalf("test did not cross stage deadline during recovery: %v", ctx.Err())
	}
	logs, err := f.h.Logs(id)
	if err != nil {
		f.t.Fatal(err)
	}
	warning := false
	for line := range logs {
		warning = warning || strings.Contains(line.Text, "authentication writeback skipped:") ||
			strings.Contains(line.Text, "authentication log recovery failed:")
	}
	if !warning {
		f.t.Error("credential recovery failure was not logged")
	}
	exit, session := 0, string(result.SessionID)
	return runner.StageOutcome{ExitCode: &exit, Work: &runner.StageWork{SessionID: &session}}
}

func workStageFrame(t *testing.T, f *fixture) (*runner.Runner, runner.Claim) {
	t.Helper()
	root, data := filepath.Dir(f.env.Worktree), t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.WriteFile(filepath.Join(repo, "stage.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("add", "stage.sh")
	git("commit", "--quiet", "-m", "stage fixture")
	git("tag", "profile/v1.0.0")
	definition, err := json.Marshal(contract.ProfileDefinition{
		ContractVersion: "1", ProfileVersion: "1.0.0", Name: "test", AcceptanceCriteria: "optional",
		Stages: contract.ProfileStages{
			Work:    contract.WorkStage{MicroHarness: model.AdapterCodex, Model: "test-model", TimeoutSec: 1},
			Check:   contract.ProgramStage{Command: []string{"stage.sh"}, TimeoutSec: 1},
			Publish: contract.ProgramStage{Command: []string{"stage.sh"}, TimeoutSec: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var claim runner.Claim
	claim.Execution.ID, claim.Execution.Seq, claim.Execution.Mode = ulid.Make().String(), 1, "continue"
	claim.Task.ID, claim.Task.AcceptanceCriteria = "task", "one commit"
	claim.Task.Source = json.RawMessage(`{"kind":"linear","identifier":"ENG-1","url":"https://linear.app/test"}`)
	claim.Route, claim.Params = json.RawMessage(`{"kind":"project","projectId":"project","projectName":"Project"}`), json.RawMessage(`{}`)
	claim.Profile.HarnessID, claim.Profile.Version, claim.Profile.TagName = "harness", "1.0.0", "profile/v1.0.0"
	claim.Profile.CommitSHA, claim.Profile.RepoPath, claim.Profile.Definition = git("rev-parse", "HEAD"), repo, definition
	claim.Workspace.ID, claim.Workspace.Path = filepath.Base(f.env.Worktree), f.env.Worktree
	claim.Limits.TimeoutSec = map[string]int{"work": 1, "check": 1, "publish": 1}
	token := filepath.Join(data, "token")
	if err := os.WriteFile(token, []byte(base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{
		RunnerID: "test-runner", ControlPlaneURL: "http://127.0.0.1", TokenFile: token,
		WorkspaceRoot: root, DataDir: data, SecretsDir: filepath.Join(data, "secrets"), MicroHarnessAuthFiles: map[string]string{},
		Container: runner.ContainerConfig{RuntimePath: filepath.Join(root, "runtime"), ProxyBinary: filepath.Join(root, "proxy"),
			Image: "test-image", ProxyImage: "test-proxy", CPUs: 4, MemoryMiB: 8192},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(data, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
	})
	return r, claim
}
