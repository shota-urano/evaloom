package runner

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Keep the expected sequence independent of ExecutionStages: accidentally
// dropping/reordering a production table row must fail the acceptance tests.
var expectedStageIDs = []StageID{
	StageValidateProject, StageCheckPreviousPR, StagePrepareWorktree,
	StagePinContext, StageReceiveContext, StageSetup, StageTestBaseline,
	StageHarness, StageHarnessHead, StageGate1, StageGate2, StageGate3,
	StagePublish, StageComplete,
}

func stubStageHandlers(run func(StageID, context.Context, *Execution) error) map[StageID]StageFunc {
	handlers := make(map[StageID]StageFunc)
	for _, id := range expectedStageIDs {
		handlers[id] = func(ctx context.Context, execution *Execution) error {
			return run(id, ctx, execution)
		}
	}
	return handlers
}

func TestExecutionStageTable(t *testing.T) {
	table := ExecutionStages()
	if len(table) != 14 {
		t.Fatalf("got %d stages; want 14", len(table))
	}
	for i, stage := range table {
		status := model.ExecutionPreparing
		switch {
		case i == 7:
			status = model.ExecutionRunning
		case i >= 8 && i <= 11:
			status = model.ExecutionVerifying
		case i == 12:
			status = model.ExecutionPublishing
		case i == 13:
			status = model.ExecutionSucceeded
		}
		want := Stage{Number: i + 1, ID: expectedStageIDs[i], Status: status}
		if stage != want {
			t.Errorf("row %d = %+v; want %+v", i+1, stage, want)
		}
	}
}

func TestExecutorOrderAndResume(t *testing.T) {
	for _, tc := range []struct {
		name   string
		resume *string
		start  int
	}{
		{"full", nil, 0},
		{"gate2_starts_at_11", new("gate2"), 10},
		{"gate3_starts_at_12", new("gate3"), 11},
		{"publish_starts_at_13", new("publish"), 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var called []StageID
			var firstExecution *Execution
			ctx := context.Background()
			claim := Claim{}
			claim.Execution.ID = "execution-1"
			claim.Execution.ResumePoint = tc.resume
			executor, err := NewExecutor(stubStageHandlers(func(id StageID, gotCtx context.Context, execution *Execution) error {
				if firstExecution == nil {
					firstExecution = execution
				}
				if execution != firstExecution || !reflect.DeepEqual(execution.Claim, claim) || gotCtx != ctx {
					t.Error("stage lost the claim, context, or shared execution data")
				}
				called = append(called, id)
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if err := executor.Execute(ctx, claim); err != nil {
				t.Fatal(err)
			}
			if want := expectedStageIDs[tc.start:]; !reflect.DeepEqual(called, want) {
				t.Fatalf("called %v; want %v", called, want)
			}
		})
	}
}

func TestExecutorStopsAtFailingStage(t *testing.T) {
	for i, failing := range expectedStageIDs {
		t.Run(string(failing), func(t *testing.T) {
			failure := errors.New("stage-specific failure or block")
			var called []StageID
			executor, err := NewExecutor(stubStageHandlers(func(id StageID, _ context.Context, _ *Execution) error {
				called = append(called, id)
				if id == failing {
					return failure
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			err = executor.Execute(context.Background(), Claim{})
			var stageErr *StageError
			if !errors.Is(err, failure) || !errors.As(err, &stageErr) || stageErr.Stage.ID != failing || stageErr.Stage.Number != i+1 {
				t.Fatalf("lost failing stage or original error: %v", err)
			}
			if want := expectedStageIDs[:i+1]; !reflect.DeepEqual(called, want) {
				t.Fatalf("called %v; want %v (no retry or later stages)", called, want)
			}
		})
	}
}

func TestExecutorRejectsInvalidResumeBeforeAnyStage(t *testing.T) {
	for _, resume := range []string{"", "gate1", "unknown", "Gate2"} {
		t.Run(resume, func(t *testing.T) {
			executor, err := NewExecutor(stubStageHandlers(func(StageID, context.Context, *Execution) error {
				t.Error("invalid resume point invoked a stage")
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			claim := Claim{}
			claim.Execution.ResumePoint = &resume
			if err := executor.Execute(context.Background(), claim); err == nil {
				t.Fatal("invalid resume point accepted")
			}
		})
	}
}

func TestExecutorRequiresAllHandlers(t *testing.T) {
	for _, change := range []func(map[StageID]StageFunc){
		func(handlers map[StageID]StageFunc) { delete(handlers, StageGate1) },
		func(handlers map[StageID]StageFunc) { handlers[StageGate2] = nil },
		func(handlers map[StageID]StageFunc) { handlers["unknown"] = handlers[StagePublish] },
	} {
		handlers := stubStageHandlers(func(StageID, context.Context, *Execution) error { return nil })
		change(handlers)
		if _, err := NewExecutor(handlers); err == nil {
			t.Fatal("invalid handler configuration accepted")
		}
	}
	if err := (&Executor{}).Execute(context.Background(), Claim{}); err == nil {
		t.Fatal("unconfigured executor succeeded")
	}
}

func TestExecutorHonorsContextBetweenStages(t *testing.T) {
	for _, cancelBeforeStart := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelBeforeStart {
			cancel()
		}
		var called []StageID
		executor, err := NewExecutor(stubStageHandlers(func(id StageID, _ context.Context, _ *Execution) error {
			called = append(called, id)
			cancel()
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if err := executor.Execute(ctx, Claim{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("lost context cancellation: %v", err)
		}
		cancel()
		want := []StageID(nil)
		if !cancelBeforeStart {
			want = []StageID{StageValidateProject}
		}
		if !reflect.DeepEqual(called, want) {
			t.Fatalf("called %v after cancellation; want %v", called, want)
		}
	}
}

func TestPullPassesResumePointToExecutor(t *testing.T) {
	var claims atomic.Int32
	process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			io.WriteString(w, `{"cancelExecutionIds":[]}`)
			return
		}
		if claims.Add(1) == 1 {
			io.WriteString(w, `{"execution":{"id":"resumed","taskId":"task","resumePoint":"gate2"}}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	called := make(chan StageID, 14)
	executor, err := NewExecutor(stubStageHandlers(func(id StageID, _ context.Context, execution *Execution) error {
		if execution.Claim.Execution.ID != "resumed" {
			t.Error("executor lost claimed Execution ID")
		}
		called <- id
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	startPull(t, process, func(ctx context.Context, claim Claim) {
		done <- executor.Execute(ctx, claim)
	}, func(err error) { t.Error(err) })
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
	var got []StageID
	for len(called) > 0 {
		got = append(got, <-called)
	}
	if want := expectedStageIDs[10:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("pull/executor called %v; want %v", got, want)
	}
}
