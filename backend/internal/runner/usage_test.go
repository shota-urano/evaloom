package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestStageDurationMeasuredForEveryStageAndInterruption(t *testing.T) {
	for _, stage := range executionStageOrder {
		for _, code := range []string{"", "timeout", "cancelled"} {
			t.Run(fmt.Sprintf("%s/%s", stage, code), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					executor := usageExecutor{stageExecutorFunc(func(context.Context, StageInvocation) StageOutcome {
						time.Sleep(250 * time.Millisecond)
						outcome := StageOutcome{Usage: StageUsage{WallTimeMS: 9999}}
						if code != "" {
							outcome.RunnerError = &code
						}
						return outcome
					})}
					got := executor.RunStage(t.Context(), StageInvocation{Stage: stage})
					if got.Usage.WallTimeMS != 250 || (got.RunnerError != nil) != (code != "") ||
						(got.RunnerError != nil && *got.RunnerError != code) {
						t.Fatalf("duration or interruption changed: %+v", got)
					}
				})
			})
		}
	}
}

func TestProgramStageUsage(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		known       bool
	}{
		{"unknown", `null`, false},
		{"null-fields", `{"inputTokens":null,"outputTokens":null,"costAmount":null}`, false},
		{"zero", `{"inputTokens":0,"outputTokens":0,"costAmount":0}`, true},
		{"invalid-type", `{"inputTokens":"bad","outputTokens":5,"costAmount":0}`, false},
		{"negative", `{"inputTokens":-1,"outputTokens":5,"costAmount":0}`, false},
		{"overflow", `{"inputTokens":9223372036854775808,"outputTokens":5,"costAmount":0}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(fmt.Sprintf(`{"contractVersion":"1","stage":"prepare","status":"succeeded","message":"ok","usage":%s}`, tc.usage))
			got := programStageUsage(raw, 1234)
			if got.WallTimeMS != 1234 || (got.InputTokens != nil) != tc.known ||
				(got.OutputTokens != nil) != tc.known || (got.CostAmount != nil) != tc.known {
				t.Fatalf("usage=%+v", got)
			}
			if tc.known && (*got.InputTokens != 0 || *got.OutputTokens != 0 || *got.CostAmount != 0) {
				t.Fatalf("known zeros changed: %+v", got)
			}
		})
	}
}

func TestWorkStageUsageFromAdapter(t *testing.T) {
	for _, class := range []adapter.ErrorClass{"", adapter.ErrorTimeout, adapter.ErrorCancelled} {
		for _, known := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/known=%v", class, known), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					result := adapter.HarnessResult{Status: adapter.ResultSucceeded, Usage: adapter.Usage{WallTimeMS: 9999}}
					if known {
						result.Usage.InputTokens, result.Usage.OutputTokens = stagePtr(int64(17)), stagePtr(int64(0))
					}
					if class != "" {
						result.Status, result.ErrorClass = adapter.ResultFailed, &class
					}
					executor := usageExecutor{stageExecutorFunc(func(ctx context.Context, in StageInvocation) StageOutcome {
						_, err := in.WaitWork(ctx, &finishingAdapter{result: result}, "session")
						var want error
						if class == adapter.ErrorTimeout {
							want = context.DeadlineExceeded
						} else if class == adapter.ErrorCancelled {
							want = context.Canceled
						}
						if !errors.Is(err, want) {
							t.Fatalf("WaitWork error=%v, want %v", err, want)
						}
						return StageOutcome{}
					})}
					got := executor.RunStage(t.Context(), StageInvocation{Stage: model.StageWork}).Usage
					if got.WallTimeMS != 1200 || (got.InputTokens != nil) != known ||
						(got.OutputTokens != nil) != known || got.CostAmount != nil {
						t.Fatalf("usage=%+v", got)
					}
					if known && (*got.InputTokens != 17 || *got.OutputTokens != 0) {
						t.Fatalf("Adapter counts changed: %+v", got)
					}
				})
			})
		}
	}
}

func TestStageUsageReachesControlPlane(t *testing.T) {
	for _, mode := range []string{"unknown", "known", "failed-check"} {
		t.Run(mode, func(t *testing.T) {
			f := newLogControlPlane(t)
			r, claim, _, services := stageFixture(t, true)
			process, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			r.controlPlaneURL, r.client, r.token = process.controlPlaneURL, process.client, process.token
			claim.Execution.ID, claim.Execution.TaskID, claim.Task.ID = logExecution, "task-001", "task-001"
			if _, err := f.db.Exec(`UPDATE harness_profiles SET definition = ? WHERE id = 'fixture-profile'`, string(claim.Profile.Definition)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE executions SET status = 'claimed', profile_id = 'fixture-profile',
				route_kind = 'project', project_id = 'project', harness_id = 'fixture-harness' WHERE id = ?`, logExecution); err != nil {
				t.Fatal(err)
			}
			services.Reporter = r
			known := mode != "unknown"
			services.Executor = stageExecutorFunc(func(ctx context.Context, in StageInvocation) StageOutcome {
				time.Sleep(15 * time.Millisecond)
				outcome := successfulStage(in.Stage)
				if in.Stage == model.StageWork {
					result := adapter.HarnessResult{Status: adapter.ResultSucceeded, Usage: adapter.Usage{WallTimeMS: 9999}}
					if known {
						result.Usage.InputTokens, result.Usage.OutputTokens = stagePtr(int64(17)), stagePtr(int64(0))
					}
					if _, err := in.WaitWork(ctx, &finishingAdapter{result: result}, "session"); err != nil {
						t.Fatal(err)
					}
				} else {
					result := contract.Result{ContractVersion: "1", Stage: in.Stage, Status: model.StageSucceeded, Message: "ok"}
					if in.Stage == model.StageCheck {
						result.ContentID = stagePtr("checked")
						if mode == "failed-check" {
							result.Status, result.ErrorClass = model.StageFailed, stagePtr(model.ErrorTestFailed)
						}
					}
					if known {
						result.Usage = &contract.Usage{InputTokens: stagePtr(int64(17)), OutputTokens: stagePtr(int64(0)), CostAmount: stagePtr(0.125)}
					}
					outcome.Result, err = json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
				}
				return outcome
			})
			started := time.Now()
			if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(started).Milliseconds()
			rows, err := f.db.Query(`SELECT stage, wall_time_ms, input_tokens, output_tokens, cost_amount FROM usage_records WHERE execution_id = ?`, logExecution)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			seen := map[model.Stage]bool{}
			for rows.Next() {
				var stage model.Stage
				var wall int64
				var input, output sql.NullInt64
				var cost sql.NullFloat64
				if err := rows.Scan(&stage, &wall, &input, &output, &cost); err != nil {
					t.Fatal(err)
				}
				if seen[stage] || wall < 15 || wall > elapsed || input.Valid != known || output.Valid != known ||
					cost.Valid != (known && stage != model.StageWork) {
					t.Fatalf("stage=%s wall=%d input=%+v output=%+v cost=%+v", stage, wall, input, output, cost)
				}
				if known && (input.Int64 != 17 || output.Int64 != 0 || (cost.Valid && cost.Float64 != 0.125)) {
					t.Fatalf("stored usage differs: %s %+v %+v %+v", stage, input, output, cost)
				}
				seen[stage] = true
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			want := executionStageOrder
			status := "succeeded"
			if mode == "failed-check" {
				want, status = want[:3], "failed"
			}
			if len(seen) != len(want) {
				t.Fatalf("usage rows=%v, want %v", seen, want)
			}
			for _, stage := range want {
				if !seen[stage] {
					t.Fatalf("missing usage for %s", stage)
				}
			}
			var actual string
			if err := f.db.QueryRow(`SELECT status FROM executions WHERE id = ?`, logExecution).Scan(&actual); err != nil || actual != status {
				t.Fatalf("status=%s want=%s err=%v", actual, status, err)
			}
		})
	}
}
