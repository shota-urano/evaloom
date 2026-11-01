package codex

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

type stoppedProcess struct{ err error }

func (p stoppedProcess) Wait() error           { return p.err }
func (p stoppedProcess) Stop() error           { return nil }
func (p stoppedProcess) Kill() error           { return nil }
func (p stoppedProcess) ExitCode() (int, bool) { return 0, false }

func TestCodexSandboxDeadlineClassification(t *testing.T) {
	for _, phase := range []string{"launch", "wait"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t, "success")
			f.config.Launch = func(context.Context, adapter.SandboxEnv, []string, io.Writer, io.Writer) (sandbox.Process, error) {
				err := &sandbox.Error{Op: "sandbox deadline", Cause: context.DeadlineExceeded}
				if phase == "launch" {
					return nil, err
				}
				return stoppedProcess{err: err}, nil
			}
			f.config.CancelGrace = time.Second
			f.h = New(f.config)
			_, r, _ := runResult(t, f)
			if r.Status != adapter.ResultFailed || r.ErrorClass == nil || *r.ErrorClass != adapter.ErrorTimeout {
				t.Fatalf("result=%+v", r)
			}
		})
	}
}
