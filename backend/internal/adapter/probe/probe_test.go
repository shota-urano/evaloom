package probe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

type sandboxStub struct {
	checkErr, runErr, readErr error
	exit                      int
	report                    string
	block                     bool
	runs                      int
}

func (s *sandboxStub) Check(context.Context) error { return s.checkErr }
func (s *sandboxStub) Run(ctx context.Context, env adapter.SandboxEnv, cmd []string) (Output, error) {
	s.runs++
	if s.block {
		<-ctx.Done()
		return Output{}, ctx.Err()
	}
	return Output{ExitCode: s.exit, Stdout: "probe output", Stderr: "probe diagnostic"}, s.runErr
}
func (s *sandboxStub) ReadFile(context.Context, adapter.SandboxEnv, string) ([]byte, error) {
	return []byte(s.report), s.readErr
}

const validReport = `{"observations":[{"id":"access","kind":"read","succeeded":false,"detail":{}}]}`

func request(t *testing.T) adapter.HarnessRequest {
	t.Helper()
	data, err := os.ReadFile("../testdata/harness_request.json")
	if err != nil {
		t.Fatal(err)
	}
	var req adapter.HarnessRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func wait(t *testing.T, h adapter.Adapter, id adapter.SessionID) adapter.HarnessResult {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := h.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		if status != adapter.SessionRunning {
			result, err := h.Result(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := result.Validate(); err != nil {
				t.Fatal(err)
			}
			return result
		}
		select {
		case <-deadline.C:
			t.Fatal("probe did not stop")
		case <-ticker.C:
		}
	}
}

func TestProbeFailsClosedWithoutPrerequisites(t *testing.T) {
	req := request(t)
	broken := &sandboxStub{checkErr: errors.New("not installed")}
	for _, sandbox := range []Sandbox{nil, broken} {
		if _, err := New(sandbox).Start(context.Background(), req, adapter.SandboxEnv{Worktree: req.Project.Workdir}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("missing prerequisite must fail: %v", err)
		}
	}
	if broken.runs != 0 {
		t.Fatal("ran despite missing prerequisite")
	}
}

func TestProbeResultsAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sandbox sandboxStub
		class   adapter.ErrorClass
	}{
		{"success_even_when_access_denied", sandboxStub{report: validReport}, ""},
		{"sandbox_failed", sandboxStub{runErr: errors.New("sandbox failed")}, adapter.ErrorSandbox},
		{"script_failed", sandboxStub{exit: 1}, adapter.ErrorExecutionFailed},
		{"result_absent", sandboxStub{readErr: os.ErrNotExist}, adapter.ErrorExecutionFailed},
		{"result_invalid", sandboxStub{report: `{}`}, adapter.ErrorExecutionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(&tc.sandbox)
			req := request(t)
			id, err := h.Start(context.Background(), req, adapter.SandboxEnv{Worktree: req.Project.Workdir})
			if err != nil {
				t.Fatal(err)
			}
			result := wait(t, h, id)
			if tc.class == "" {
				if result.Status != adapter.ResultSucceeded || result.ErrorClass != nil {
					t.Fatalf("%+v", result)
				}
			} else if result.Status != adapter.ResultFailed || result.ErrorClass == nil || *result.ErrorClass != tc.class {
				t.Fatalf("%+v", result)
			}
			logs, err := h.Logs(id)
			if err != nil {
				t.Fatal(err)
			}
			if line := <-logs; line.Stream != "stdout" || line.Text != "probe output" {
				t.Fatalf("%+v", line)
			}
			if h.Capabilities().Streaming {
				t.Fatal("buffered logs must not claim streaming")
			}
		})
	}
}

func TestProbeCancellationAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		h := New(&sandboxStub{block: true})
		req := request(t)
		id, err := h.Start(ctx, req, adapter.SandboxEnv{Worktree: req.Project.Workdir})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if _, err := h.Result(id); err == nil {
			t.Fatal("running result returned")
		}
		if !timeout {
			if err := h.Cancel(id); err != nil {
				t.Fatal(err)
			}
		}
		result := wait(t, h, id)
		cancel()
		want := adapter.ErrorCancelled
		if timeout {
			want = adapter.ErrorTimeout
		}
		if result.ErrorClass == nil || *result.ErrorClass != want {
			t.Fatalf("%+v", result)
		}
	}
	h := New(nil)
	if _, err := h.Status("unknown"); err == nil {
		t.Fatal("unknown session")
	}
	if _, err := h.Result("unknown"); err == nil {
		t.Fatal("unknown session")
	}
	if _, err := h.Logs("unknown"); err == nil {
		t.Fatal("unknown session")
	}
	if err := h.Cancel("unknown"); err == nil {
		t.Fatal("unknown session")
	}
}

func TestDecodeReportRejectsIncompleteEvidence(t *testing.T) {
	for _, data := range []string{"", `{}`, `{"observations":[]}`, `{"observations":[{"id":"a","kind":"read"}]}`, validReport + ` {}`, validReport[:len(validReport)-1],
		`{"observations":[{"id":"a","kind":"read","succeeded":true},{"id":"a","kind":"read","succeeded":false}]}`} {
		if _, err := DecodeReport([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}
