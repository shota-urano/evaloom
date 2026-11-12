package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

type stageExecutorFunc func(context.Context, StageInvocation) StageOutcome

func (f stageExecutorFunc) RunStage(ctx context.Context, in StageInvocation) StageOutcome {
	return f(ctx, in)
}

type stageReportStub struct {
	requests  map[model.Stage][]byte
	started   []model.Stage
	finished  []model.Stage
	outcomes  []StageOutcome
	attempts  map[model.Stage]int
	next      func(model.Stage, StageOutcome) StageNext
	startErr  error
	finishErr error
}

func (s *stageReportStub) StartStage(_ context.Context, _ string, stage model.Stage, request []byte) (int, error) {
	s.started = append(s.started, stage)
	s.requests[stage] = bytes.Clone(request)
	if n := s.attempts[stage]; n > 0 {
		return n, s.startErr
	}
	return 1, s.startErr
}

func (s *stageReportStub) FinishStage(_ context.Context, _ string, stage model.Stage, _ int, outcome StageOutcome) (StageNext, error) {
	s.finished = append(s.finished, stage)
	s.outcomes = append(s.outcomes, outcome)
	if s.next != nil {
		return s.next(stage, outcome), s.finishErr
	}
	if outcome.RunnerError != nil || stage == model.StagePublish {
		return StageNext{Next: "stop"}, s.finishErr
	}
	next := StageNext{Next: string(executionStageOrder[stageIndex(stage)+1])}
	if next.Next == "publish" {
		next.CheckedContentID = stagePtr("checked-by-control-plane")
		next.Approvals = []contract.RequestApproval{{Operation: "send", Status: "waived"}}
	}
	return next, s.finishErr
}

func stagePtr[T any](value T) *T { return &value }

func stageFixture(t *testing.T, prepare bool) (*Runner, Claim, *stageReportStub, StageServices) {
	t.Helper()
	f := newProfileFixture(t)
	if !prepare {
		f.def.Stages.Prepare = nil
	}
	// Declarations are intentionally present, but values are never request data.
	f.def.Stages.Publish.Secrets = []contract.Secret{{Slot: "delivery", Access: "write"}}
	f.definition()
	r := newProfileRunner(t)
	r.dataDir = channelDirectory(t)
	r.token = "runner-private-token"
	c := f.claim
	c.Execution.ID, c.Execution.TaskID, c.Execution.Seq, c.Execution.Mode = ulid.Make().String(), "task-1", 2, "continue"
	c.Task.ID, c.Task.Title, c.Task.Body, c.Task.AcceptanceCriteria = "task-1", "title", "body; $(must-stay-text)", "criteria"
	c.Task.Source = json.RawMessage(`{"kind":"linear","identifier":"ENG-1","url":"https://linear.app/test"}`)
	c.Route = json.RawMessage(`{"kind":"project","projectId":"project-1","projectName":"Project"}`)
	c.Params = json.RawMessage(`{"opaque":{"command":"do not execute","path":"../not-a-path"}}`)
	c.Profile.HarnessID = "test-harness"
	if err := secretstore.New(r.dataDir).Set(c.Profile.HarnessID, "delivery", "write", []byte("delivery-private-value")); err != nil {
		t.Fatal(err)
	}
	r.workspaceRoot = t.TempDir()
	c.Workspace.ID = ulid.Make().String()
	c.Workspace.Path = filepath.Join(r.workspaceRoot, c.Workspace.ID)
	if err := os.Mkdir(c.Workspace.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Workspace.Path, "keep"), []byte("workspace survives"), 0600); err != nil {
		t.Fatal(err)
	}
	c.Limits.TimeoutSec = map[string]int{"prepare": 11, "work": 22, "check": 33, "publish": 44}
	c.Previous = json.RawMessage(`{"executionId":"previous-id","checkedContentId":"previous-content","outputs":[{"kind":"document","label":"document","url":"https://example.test/document"}]}`)
	s := &stageReportStub{requests: map[model.Stage][]byte{}, attempts: map[model.Stage]int{}}
	services := StageServices{Reporter: s, Paths: contract.RequestPaths{
		Profile: "/sandbox/profile", Workspace: "/sandbox/workspace", Out: "/sandbox/out",
		Inputs: "/sandbox/inputs", Records: "/sandbox/records", Scratch: "/sandbox/scratch",
	}}
	services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
		writeStageOutput(t, in)
		return successfulStage(in.Stage)
	})
	return r, c, s, services
}

func successfulStage(stage model.Stage) StageOutcome {
	out := StageOutcome{ExitCode: stagePtr(0), Usage: StageUsage{WallTimeMS: 123}}
	if stage == model.StageWork {
		out.Work = &StageWork{SessionID: stagePtr("work-session")}
	} else {
		out.Result = json.RawMessage(fmt.Sprintf(`{"contractVersion":"1","stage":%q,"status":"succeeded","message":"ok"}`, stage))
	}
	return out
}

func writeStageOutput(t *testing.T, in StageInvocation) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(in.Directory, "out", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in.Directory, "out", "nested", "data"), []byte(in.Stage), 0600); err != nil {
		t.Fatal(err)
	}
}

func readStageRequest(t *testing.T, raw []byte) contract.Request {
	t.Helper()
	var request contract.Request
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatalf("request violates contract 6.1: %v\n%s", err, raw)
	}
	return request
}

func TestExecuteStagesOrderAndRequestContract(t *testing.T) {
	for _, prepare := range []bool{true, false} {
		t.Run(fmt.Sprintf("prepare=%v", prepare), func(t *testing.T) {
			r, claim, reporter, services := stageFixture(t, prepare)
			t.Setenv("EVALOOM_SECRET_DELIVERY", "delivery-private-value")
			var called []model.Stage
			services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
				if in.Attempt != 1 || in.Workspace != claim.Workspace.Path || in.Profile.Path == "" {
					t.Fatalf("wrong invocation: %+v", in)
				}
				raw, err := os.ReadFile(filepath.Join(in.Directory, "request.json"))
				if err != nil || !bytes.Equal(raw, reporter.requests[in.Stage]) {
					t.Fatalf("saved request differs from registered request: %s, %v", raw, err)
				}
				info, err := os.Stat(filepath.Join(in.Directory, "request.json"))
				if err != nil || info.Mode().Perm()&0222 != 0 {
					t.Fatalf("request must be read-only: %v", err)
				}
				req := readStageRequest(t, raw)
				if req.Stage != in.Stage || req.Execution.ID != claim.Execution.ID || req.Execution.Seq != 2 || req.Execution.Mode != model.ModeContinue || req.Execution.ResumeStage != nil ||
					req.Task.ID != claim.Task.ID || req.Task.Title != claim.Task.Title || req.Task.Body != claim.Task.Body || req.Task.AcceptanceCriteria != claim.Task.AcceptanceCriteria ||
					req.Task.Source.Kind != model.SourceLinear || *req.Task.Source.Identifier != "ENG-1" || *req.Task.Source.URL != "https://linear.app/test" ||
					req.Route.Kind != model.RouteProject || *req.Route.ProjectID != "project-1" || *req.Route.ProjectName != "Project" ||
					!bytes.Equal(req.Params, claim.Params) || req.Profile.HarnessID != claim.Profile.HarnessID || req.Profile.Version != claim.Profile.Version || req.Profile.CommitSHA != claim.Profile.CommitSHA ||
					req.Paths != services.Paths || req.Limits.TimeoutSec != claim.Limits.TimeoutSec[string(in.Stage)] || req.Answer != nil ||
					req.Previous == nil || req.Previous.ExecutionID != "previous-id" || *req.Previous.CheckedContentID != "previous-content" || len(req.Previous.Outputs) != 1 {
					t.Fatalf("claim data was not preserved: %s", raw)
				}
				if in.Stage == model.StagePublish && (req.CheckedContentID == nil || *req.CheckedContentID != "checked-by-control-plane" || len(req.Approvals) != 1 || req.Approvals[0].Status != "waived") {
					t.Fatalf("publish must use the finish reply: %s", raw)
				}
				for _, forbidden := range []string{r.token, "delivery-private-value", "EVALOOM_SECRET_", "repoPath", "definition", "secrets"} {
					if bytes.Contains(raw, []byte(forbidden)) {
						t.Fatalf("request leaked %q", forbidden)
					}
				}
				entries, err := os.ReadDir(filepath.Join(in.Directory, "inputs"))
				if err != nil || len(entries) != len(called) {
					t.Fatalf("inputs must contain just prior stages: %v, %v", entries, err)
				}
				for _, prior := range called {
					path := filepath.Join(in.Directory, "inputs", string(prior), "nested", "data")
					data, err := os.ReadFile(path)
					info, statErr := os.Stat(path)
					if err != nil || string(data) != string(prior) || statErr != nil || info.Mode().Perm()&0222 != 0 {
						t.Fatalf("prior out must be preserved read-only: %s, %v, %v", data, err, statErr)
					}
				}
				called = append(called, in.Stage)
				writeStageOutput(t, in)
				return successfulStage(in.Stage)
			})
			if err := r.ExecuteStages(context.Background(), claim, services); err != nil {
				t.Fatal(err)
			}
			want := []model.Stage{model.StagePrepare, model.StageWork, model.StageCheck, model.StagePublish}
			if !prepare {
				want = want[1:]
			}
			if !slices.Equal(called, want) || !slices.Equal(reporter.started, want) || !slices.Equal(reporter.finished, want) {
				t.Fatalf("wrong stage order: run=%v start=%v finish=%v", called, reporter.started, reporter.finished)
			}
		})
	}
}

func TestExecuteStagesNextStopAtEveryStage(t *testing.T) {
	for index, stop := range executionStageOrder {
		t.Run(string(stop), func(t *testing.T) {
			r, claim, reporter, services := stageFixture(t, true)
			reporter.next = func(stage model.Stage, _ StageOutcome) StageNext {
				if stage == stop {
					return StageNext{Next: "stop"}
				}
				return StageNext{Next: string(executionStageOrder[stageIndex(stage)+1]), CheckedContentID: stagePtr("checked")}
			}
			if err := r.ExecuteStages(context.Background(), claim, services); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(reporter.started, executionStageOrder[:index+1]) || !slices.Equal(reporter.started, reporter.finished) {
				t.Fatalf("continued past stop: %v", reporter.started)
			}
		})
	}
}

func setStageResume(t *testing.T, claim *Claim, resume stageResume) {
	t.Helper()
	claim.Execution.ResumeStage = stagePtr(string(resume.Stage))
	raw, err := json.Marshal(resume)
	if err != nil {
		t.Fatal(err)
	}
	claim.Resume = raw
}

func TestExecuteStagesResumePublishAfterRunnerRestart(t *testing.T) {
	r, claim, reporter, services := stageFixture(t, true)
	reporter.next = func(stage model.Stage, _ StageOutcome) StageNext {
		if stage == model.StageCheck {
			return StageNext{Next: "stop"} // waiting_approval
		}
		return StageNext{Next: string(executionStageOrder[stageIndex(stage)+1])}
	}
	if err := r.ExecuteStages(context.Background(), claim, services); err != nil {
		t.Fatal(err)
	}
	approved := contract.RequestApproval{ID: stagePtr("approval-id"), Operation: "send", ContentID: stagePtr("checked"), Status: "approved"}
	setStageResume(t, &claim, stageResume{Stage: model.StagePublish, CheckedContentID: stagePtr("checked"), Approvals: []contract.RequestApproval{approved}, CompletedStages: executionStageOrder[:3]})
	restarted := &Runner{dataDir: r.dataDir, workspaceRoot: r.workspaceRoot}
	resumedReports := &stageReportStub{requests: map[model.Stage][]byte{}}
	services.Reporter = resumedReports
	var called []model.Stage
	services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
		called = append(called, in.Stage)
		req := readStageRequest(t, resumedReports.requests[in.Stage])
		if req.Execution.ResumeStage == nil || *req.Execution.ResumeStage != model.StagePublish || req.CheckedContentID == nil || *req.CheckedContentID != "checked" || !reflect.DeepEqual(req.Approvals, []contract.RequestApproval{approved}) || req.Answer != nil {
			t.Fatalf("wrong resumed publish request: %+v", req)
		}
		data, err := os.ReadFile(filepath.Join(in.Directory, "inputs", "check", "nested", "data"))
		if err != nil || string(data) != "check" {
			t.Fatalf("prior check out was not restored: %s, %v", data, err)
		}
		return successfulStage(in.Stage)
	})
	if err := restarted.ExecuteStages(context.Background(), claim, services); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(called, []model.Stage{model.StagePublish}) || !slices.Equal(resumedReports.started, called) {
		t.Fatalf("publish resume ran other stages: %v", called)
	}
}

func TestExecuteStagesResumeWorkAnswerAndAttempt(t *testing.T) {
	r, claim, reporter, services := stageFixture(t, true)
	reporter.next = func(stage model.Stage, _ StageOutcome) StageNext {
		if stage == model.StageWork {
			return StageNext{Next: "stop"} // question
		}
		return StageNext{Next: "work"}
	}
	if err := r.ExecuteStages(context.Background(), claim, services); err != nil {
		t.Fatal(err)
	}
	answer := &contract.Answer{DecisionID: "decision-id", Text: "the answer", Option: stagePtr("option")}
	setStageResume(t, &claim, stageResume{Stage: model.StageWork, Answer: answer, WorkSessionID: stagePtr("original-session"), CompletedStages: []model.Stage{model.StagePrepare}})
	reports := &stageReportStub{requests: map[model.Stage][]byte{}, attempts: map[model.Stage]int{model.StageWork: 2}}
	services.Reporter = reports
	services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
		req := readStageRequest(t, reports.requests[in.Stage])
		if in.Stage == model.StageWork {
			if in.Attempt != 2 || !strings.HasSuffix(in.Directory, "work-2") || !reflect.DeepEqual(req.Answer, answer) || in.WorkSessionID == nil || *in.WorkSessionID != "original-session" {
				t.Fatalf("work resume lost attempt, answer or session: %+v, %+v", in, req)
			}
			if _, err := os.Stat(filepath.Join(in.Directory, "inputs", "work")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("question run must not become a completed work input")
			}
		} else if req.Answer != nil || in.WorkSessionID != nil {
			t.Fatal("answer/session leaked beyond work")
		}
		writeStageOutput(t, in)
		return successfulStage(in.Stage)
	})
	if err := r.ExecuteStages(context.Background(), claim, services); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reports.started, executionStageOrder[1:]) {
		t.Fatalf("work resume order: %v", reports.started)
	}
	old, err := os.ReadFile(filepath.Join(r.dataDir, "executions", claim.Execution.ID, "work-1", "out", "nested", "data"))
	if err != nil || string(old) != "work" {
		t.Fatalf("resuming overwrote earlier attempt: %s, %v", old, err)
	}
}

func TestExecuteStagesFailuresKeepWorkspaceAndOut(t *testing.T) {
	for index, failed := range executionStageOrder {
		t.Run(string(failed), func(t *testing.T) {
			r, claim, reporter, services := stageFixture(t, true)
			var called []model.Stage
			failure := successfulStage(failed)
			failure.ExitCode = stagePtr(1)
			if failed == model.StageWork {
				failure.RunnerError = stagePtr("work_failed")
			} else {
				failure.Result = json.RawMessage(fmt.Sprintf(`{"contractVersion":"1","stage":%q,"status":"failed","errorClass":"%s","message":"failure"}`, failed, map[model.Stage]string{model.StagePrepare: "prepare_failed", model.StageCheck: "check_failed", model.StagePublish: "publish_failed"}[failed]))
			}
			reporter.next = func(stage model.Stage, _ StageOutcome) StageNext {
				if stage == failed {
					return StageNext{Next: "stop"}
				}
				return StageNext{Next: string(executionStageOrder[stageIndex(stage)+1]), CheckedContentID: stagePtr("checked")}
			}
			services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
				called = append(called, in.Stage)
				writeStageOutput(t, in)
				if in.Stage == failed {
					return failure
				}
				return successfulStage(in.Stage)
			})
			if err := r.ExecuteStages(context.Background(), claim, services); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(called, executionStageOrder[:index+1]) || !reflect.DeepEqual(reporter.outcomes[index], failure) {
				t.Fatalf("failure was retried, not forwarded, or followed by a stage: %v, %+v", called, reporter.outcomes)
			}
			data, err := os.ReadFile(filepath.Join(claim.Workspace.Path, "keep"))
			if err != nil || string(data) != "workspace survives" {
				t.Fatalf("workspace not retained: %s, %v", data, err)
			}
			data, err = os.ReadFile(filepath.Join(r.dataDir, "executions", claim.Execution.ID, string(failed)+"-1", "out", "nested", "data"))
			if err != nil || string(data) != string(failed) {
				t.Fatalf("failed stage out not retained: %s, %v", data, err)
			}
		})
	}
}

func TestExecuteStagesMissingResumeOutReportsWorkspaceMissing(t *testing.T) {
	r, claim, reporter, services := stageFixture(t, false)
	setStageResume(t, &claim, stageResume{Stage: model.StagePublish, CheckedContentID: stagePtr("checked"), CompletedStages: []model.Stage{model.StageCheck}})
	services.Executor = stageExecutorFunc(func(context.Context, StageInvocation) StageOutcome {
		t.Fatal("missing prior out must prevent stage execution")
		return StageOutcome{}
	})
	if err := r.ExecuteStages(context.Background(), claim, services); err != nil {
		t.Fatal(err)
	}
	if len(reporter.outcomes) != 1 || reporter.outcomes[0].RunnerError == nil || *reporter.outcomes[0].RunnerError != "workspace_missing" {
		t.Fatalf("missing out was not reported: %+v", reporter.outcomes)
	}
}

func TestExecuteStagesResumeUsesLatestOutAndDoesNotFallBack(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("latest-missing=%v", missing), func(t *testing.T) {
			r, claim, reporter, services := stageFixture(t, false)
			for _, attempt := range []int{2, 10} {
				name := filepath.Join(r.dataDir, "executions", claim.Execution.ID, fmt.Sprintf("check-%d", attempt))
				if err := os.MkdirAll(name, 0700); err != nil {
					t.Fatal(err)
				}
				if attempt == 10 && missing {
					continue
				}
				if err := os.Mkdir(filepath.Join(name, "out"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(name, "out", "data"), []byte(strconv.Itoa(attempt)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			setStageResume(t, &claim, stageResume{Stage: model.StagePublish, CheckedContentID: stagePtr("checked"), CompletedStages: []model.Stage{model.StageCheck}})
			calls := 0
			services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
				calls++
				data, err := os.ReadFile(filepath.Join(in.Directory, "inputs", "check", "data"))
				if err != nil || string(data) != "10" {
					t.Fatalf("did not use the latest numeric attempt: %s, %v", data, err)
				}
				return successfulStage(in.Stage)
			})
			if err := r.ExecuteStages(context.Background(), claim, services); err != nil {
				t.Fatal(err)
			}
			if missing {
				if calls != 0 || len(reporter.outcomes) != 1 || reporter.outcomes[0].RunnerError == nil || *reporter.outcomes[0].RunnerError != "workspace_missing" {
					t.Fatalf("fell back to stale out: calls=%d, outcomes=%+v", calls, reporter.outcomes)
				}
			} else if calls != 1 {
				t.Fatalf("publish calls=%d", calls)
			}
		})
	}
}

func TestExecuteStagesRejectsSymlinkInPriorOut(t *testing.T) {
	r, claim, reporter, services := stageFixture(t, false)
	prior := filepath.Join(r.dataDir, "executions", claim.Execution.ID, "check-1", "out")
	if err := os.MkdirAll(prior, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(claim.Workspace.Path, "keep"), filepath.Join(prior, "leak")); err != nil {
		t.Fatal(err)
	}
	setStageResume(t, &claim, stageResume{Stage: model.StagePublish, CheckedContentID: stagePtr("checked"), CompletedStages: []model.Stage{model.StageCheck}})
	services.Executor = stageExecutorFunc(func(context.Context, StageInvocation) StageOutcome {
		t.Fatal("a symlink in out must not be followed into a publish input")
		return StageOutcome{}
	})
	if err := r.ExecuteStages(context.Background(), claim, services); err != nil {
		t.Fatal(err)
	}
	if len(reporter.outcomes) != 1 || reporter.outcomes[0].RunnerError == nil || *reporter.outcomes[0].RunnerError != "config_error" {
		t.Fatalf("unsafe input was not reported: %+v", reporter.outcomes)
	}
}

func TestExecuteStagesRejectsInvalidNextWithoutRetry(t *testing.T) {
	for _, next := range []string{"prepare", "publish", "unknown", ""} {
		t.Run(next, func(t *testing.T) {
			r, claim, reporter, services := stageFixture(t, true)
			reporter.next = func(model.Stage, StageOutcome) StageNext { return StageNext{Next: next} }
			if err := r.ExecuteStages(context.Background(), claim, services); err == nil {
				t.Fatal("accepted an invalid next stage")
			}
			if !slices.Equal(reporter.started, []model.Stage{model.StagePrepare}) {
				t.Fatalf("invalid next caused more stages: %v", reporter.started)
			}
		})
	}
}

func TestExecuteStagesReportingFailuresDoNotRerunStage(t *testing.T) {
	for _, start := range []bool{true, false} {
		t.Run(fmt.Sprintf("start=%v", start), func(t *testing.T) {
			r, claim, reporter, services := stageFixture(t, true)
			failure := errors.New("report failed")
			if start {
				reporter.startErr = failure
			} else {
				reporter.finishErr = failure
			}
			calls := 0
			services.Executor = stageExecutorFunc(func(context.Context, StageInvocation) StageOutcome {
				calls++
				return successfulStage(model.StagePrepare)
			})
			if err := r.ExecuteStages(context.Background(), claim, services); !errors.Is(err, failure) {
				t.Fatalf("lost reporting failure: %v", err)
			}
			wantCalls := 1
			if start {
				wantCalls = 0
			}
			if calls != wantCalls || len(reporter.started) != 1 {
				t.Fatalf("stage reran after reporting failure: %d, %v", calls, reporter.started)
			}
		})
	}
}
