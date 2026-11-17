package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/dummy"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestWorkStageResolvesAdapterByProfileName(t *testing.T) {
	for _, name := range []string{"codex", "claude_code", "extension_test"} {
		t.Run(name, func(t *testing.T) {
			r, claim, reporter, services := stageFixture(t, false)
			var def contract.ProfileDefinition
			if err := json.Unmarshal(claim.Profile.Definition, &def); err != nil {
				t.Fatal(err)
			}
			def.Stages.Work.MicroHarness = name
			var err error
			claim.Profile.Definition, err = json.Marshal(def)
			if err != nil {
				t.Fatal(err)
			}
			services.Adapters = &adapter.Registry{}
			selected := dummy.New()
			if err := services.Adapters.Register(name, selected); err != nil {
				t.Fatal(err)
			}
			calls := 0
			services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
				calls++
				if in.Stage == model.StageWork {
					if in.Adapter != selected {
						t.Fatalf("Runner selected %v; want registered Adapter %v", in.Adapter, selected)
					}
				} else if in.Adapter != nil {
					t.Fatal("program stage received a work Adapter")
				}
				writeStageOutput(t, in)
				return successfulStage(in.Stage)
			})
			if err := r.ExecuteStages(context.Background(), claim, services); err != nil {
				t.Fatal(err)
			}
			if calls != 3 || len(reporter.outcomes) != 3 {
				t.Fatalf("stage calls = %d, reports = %d; want work/check/publish", calls, len(reporter.outcomes))
			}
			for _, outcome := range reporter.outcomes {
				if outcome.RunnerError != nil {
					t.Fatalf("Runner failed: %s", *outcome.RunnerError)
				}
			}
		})
	}
}

func TestWorkStageRejectsUnregisteredAdapter(t *testing.T) {
	for _, registryKind := range []string{"nil", "empty", "unrelated"} {
		t.Run(registryKind, func(t *testing.T) {
			r, claim, reporter, services := stageFixture(t, false)
			services.Adapters = nil
			if registryKind != "nil" {
				services.Adapters = &adapter.Registry{}
			}
			if registryKind == "unrelated" {
				if err := services.Adapters.Register("claude_code", dummy.New()); err != nil {
					t.Fatal(err)
				}
			}
			services.Executor = stageExecutorFunc(func(context.Context, StageInvocation) StageOutcome {
				t.Fatal("unregistered Adapter reached the executor")
				return StageOutcome{}
			})
			if err := r.ExecuteStages(context.Background(), claim, services); err != nil {
				t.Fatal(err)
			}
			if len(reporter.outcomes) != 1 || reporter.started[0] != model.StageWork ||
				reporter.outcomes[0].RunnerError == nil || *reporter.outcomes[0].RunnerError != "config_error" {
				t.Fatalf("unregistered Adapter did not stop work with config_error: %+v", reporter)
			}
		})
	}
}
