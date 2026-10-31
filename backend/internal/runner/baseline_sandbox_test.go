package runner

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

type baselineRuntimeFunc func(context.Context, sandbox.SandboxSpec, []string) (sandbox.Process, error)

func (f baselineRuntimeFunc) Run(ctx context.Context, s sandbox.SandboxSpec, argv []string) (sandbox.Process, error) {
	return f(ctx, s, argv)
}

type baselineProcess struct{ waited bool }

func (p *baselineProcess) Wait() error           { p.waited = true; return &sandbox.ExitError{Code: 1} }
func (p *baselineProcess) Stop() error           { return nil }
func (p *baselineProcess) Kill() error           { return nil }
func (p *baselineProcess) ExitCode() (int, bool) { return 1, p.waited }

func TestSandboxBaselineChecksStartCommitAndUsesTestRuntime(t *testing.T) {
	path := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "--allow-empty", "-m", "baseline")
	sha := git("rev-parse", "HEAD")
	process := &baselineProcess{}
	calls, reports := 0, 0
	launcher := SandboxBaseline{
		Sandbox: baselineRuntimeFunc(func(_ context.Context, spec sandbox.SandboxSpec, argv []string) (sandbox.Process, error) {
			calls++
			if spec.Kind != sandbox.TestRun || spec.Worktree != path || len(argv) != 1 || argv[0] != "/fixture/test" {
				t.Fatalf("incorrect runtime invocation: %+v %v", spec, argv)
			}
			return process, nil
		}),
		Prepare: func(_ context.Context, request BaselineTestRequest) (sandbox.SandboxSpec, []string, error) {
			if request.Command != "fixture test command" {
				t.Fatal("Registry command was changed")
			}
			return sandbox.SandboxSpec{Kind: sandbox.TestRun, Worktree: request.WorktreePath}, []string{"/fixture/test"}, nil
		},
		OpenReport: func(context.Context, BaselineTestRequest) (io.ReadCloser, error) {
			if !process.waited {
				t.Fatal("report opened before sandbox stopped")
			}
			reports++
			return io.NopCloser(strings.NewReader("fixture report")), nil
		},
	}
	control := testExecutionControl()
	e := &Execution{control: &control}
	request := BaselineTestRequest{Kind: sandbox.TestRun, WorktreePath: path, TargetSHA: sha, Command: "fixture test command"}
	out, err := launcher.RunBaseline(context.Background(), e, request)
	if err != nil || out.TargetSHA != sha || out.ExitCode == nil || *out.ExitCode != 1 || out.Report == nil || calls != 1 || reports != 1 {
		t.Fatalf("runtime result: %+v calls=%d reports=%d err=%v", out, calls, reports, err)
	}
	out.Report.Close()
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "--allow-empty", "-m", "changed HEAD")
	changed := git("rev-parse", "HEAD")
	if _, err := launcher.RunBaseline(context.Background(), e, request); err == nil || calls != 1 || reports != 1 {
		t.Fatalf("changed HEAD triggered a baseline run: calls=%d reports=%d err=%v", calls, reports, err)
	}
	if got := git("rev-parse", "HEAD"); got != changed {
		t.Fatal("baseline reset the continued worktree")
	}
}
