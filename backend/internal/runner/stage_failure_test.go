package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

const fakeDaemonStderr = `Error response from daemon: invalid mount config for type "bind": bind source path does not exist: /socket_mnt/x`

type failingWaitProcess struct{ err error }

func (p failingWaitProcess) Wait() error         { return p.err }
func (failingWaitProcess) Stop() error           { return nil }
func (failingWaitProcess) Kill() error           { return nil }
func (failingWaitProcess) ExitCode() (int, bool) { return 0, false }

type exitedProcess struct{}

func (exitedProcess) Wait() error           { return nil }
func (exitedProcess) Stop() error           { return nil }
func (exitedProcess) Kill() error           { return nil }
func (exitedProcess) ExitCode() (int, bool) { return 0, true }

// evaloom-6pi: the stage's error keeps the runtime's stderr (errors.As) for
// launch (create/image inspect) and Wait failures; a successful program
// keeps its previous outcome without a cause.
func TestRunProgramKeepsSandboxFailureCause(t *testing.T) {
	runtimeErr := &sandbox.Error{Op: "container creation failed", Cause: &sandbox.RuntimeError{ExitCode: 1, Stderr: fakeDaemonStderr}}
	for _, tc := range []struct {
		name   string
		launch func(context.Context, sandbox.SandboxSpec, []string) (sandbox.Process, error)
		want   string
	}{
		{"create", func(context.Context, sandbox.SandboxSpec, []string) (sandbox.Process, error) { return nil, runtimeErr }, "sandbox_error"},
		{"wait", func(context.Context, sandbox.SandboxSpec, []string) (sandbox.Process, error) {
			return failingWaitProcess{&sandbox.Error{Op: "container execution state unconfirmed",
				Cause: &sandbox.RuntimeError{ExitCode: 1, Stderr: fakeDaemonStderr}}}, nil
		}, "sandbox_error"},
		{"success", func(context.Context, sandbox.SandboxSpec, []string) (sandbox.Process, error) {
			return exitedProcess{}, nil
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, claim, reports, services := stageFixture(t, false)
			reports.next = func(model.Stage, StageOutcome) StageNext { return StageNext{Next: "stop"} }
			var outcome StageOutcome
			services.Executor = stageExecutorFunc(func(ctx context.Context, in StageInvocation) StageOutcome {
				outcome = in.RunProgram(ctx, tc.launch, sandbox.SandboxSpec{})
				return outcome
			})
			claim.Execution.ResumeStage = stagePtr("publish")
			claim.Resume = []byte(`{"stage":"publish","completedStages":[],"checkedContentId":"checked","approvals":[]}`)
			if err := r.ExecuteStages(context.Background(), claim, services); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if outcome.RunnerError != nil || outcome.Err != nil || outcome.ExitCode == nil || *outcome.ExitCode != 0 {
					t.Fatalf("successful stage changed: %+v", outcome)
				}
				return
			}
			if runnerErrorValue(outcome) != tc.want {
				t.Fatalf("runnerError=%s; want %s (%v)", runnerErrorValue(outcome), tc.want, outcome.Err)
			}
			var cause *sandbox.RuntimeError
			if !errors.As(outcome.Err, &cause) || cause.ExitCode != 1 || !strings.Contains(cause.Stderr, "invalid mount config") {
				t.Fatalf("stage error lost the runtime stderr: %#v", outcome.Err)
			}
			if !strings.Contains(stageFailureReason(outcome.Err), fakeDaemonStderr) {
				t.Fatalf("reason %q", stageFailureReason(outcome.Err))
			}
		})
	}
}

// The reason line is excluded and bounded before it reaches the stage log,
// and it is delivered as stream=runner while the stage's own lines keep
// their stream. Outcomes without a cause add nothing.
func TestStageFailureNoteIsRedactedBoundedAndRunnerStream(t *testing.T) {
	f := newLogControlPlane(t)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	log, err := process.NewLog(logExecution, "check", nil, "stage-key-value")
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Read(strings.NewReader("program output\n")); err != nil {
		t.Fatal(err)
	}
	noteStageFailure(log, StageOutcome{RunnerError: strPtr("timeout")})
	noteStageFailure(log, StageOutcome{RunnerError: strPtr("sandbox_error"), Err: &sandbox.Error{Op: "container creation failed",
		Cause: &sandbox.RuntimeError{ExitCode: 1, Stderr: fakeDaemonStderr + "\n" + process.token + " stage-key-value\n-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n" + strings.Repeat("y", 5000)}}})
	if err := log.Finish(false); err != nil {
		t.Fatal(err)
	}
	if err := await(t, startLogDelivery(t, log)); err != nil {
		t.Fatal(err)
	}
	rows, err := f.db.Query(`SELECT json_extract(payload,'$.stream'), json_extract(payload,'$.text') FROM execution_events WHERE execution_id=? AND type='log' ORDER BY id`, logExecution)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][2]string
	for rows.Next() {
		var stream, text string
		if err := rows.Scan(&stream, &text); err != nil {
			t.Fatal(err)
		}
		got = append(got, [2]string{stream, text})
	}
	if len(got) != 2 || got[0] != [2]string{"check", "program output"} || got[1][0] != "runner" {
		t.Fatalf("events %q", got)
	}
	line := got[1][1]
	if !strings.HasPrefix(line, "sandbox_error: sandbox: container creation failed: runtime exit code 1: "+fakeDaemonStderr) {
		t.Fatalf("reason line %q", line)
	}
	if strings.Contains(line, process.token) || strings.Contains(line, "stage-key-value") || strings.Contains(line, "abc") ||
		!strings.Contains(line, "[REDACTED]") || len([]rune(line)) != MaxStageFailureReason {
		t.Fatalf("reason line is not excluded and bounded (%d): %q", len([]rune(line)), line)
	}
	var redacted int
	if err := f.db.QueryRow(`SELECT redacted FROM artifacts WHERE id=?`, log.id).Scan(&redacted); err != nil || redacted != 1 {
		t.Fatalf("log artifact redacted=%d err=%v", redacted, err)
	}
}

// Work Adapters reduce a Wait failure to its class; the Launch wrapper keeps
// the Sandbox error itself, but not an ordinary nonzero exit.
func TestWorkLaunchKeepsWaitFailure(t *testing.T) {
	var kept []error
	keep := func(err error) { kept = append(kept, err) }
	failure := &sandbox.Error{Op: "container execution state unconfirmed", Cause: &sandbox.RuntimeError{ExitCode: 1, Stderr: fakeDaemonStderr}}
	if err := (&reasonProcess{Process: failingWaitProcess{failure}, keep: keep}).Wait(); err != failure {
		t.Fatalf("Wait changed the error returned to the Adapter: %v", err)
	}
	if err := (&reasonProcess{Process: failingWaitProcess{&sandbox.ExitError{Code: 7}}, keep: keep}).Wait(); err == nil {
		t.Fatal("exit error lost")
	}
	if err := (&reasonProcess{Process: exitedProcess{}, keep: keep}).Wait(); err != nil {
		t.Fatal(err)
	}
	var cause *sandbox.RuntimeError
	if len(kept) != 1 || !errors.As(kept[0], &cause) || cause.Stderr != fakeDaemonStderr {
		t.Fatalf("kept %v", kept)
	}
}
