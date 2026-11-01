package publisher_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
)

type previousPRFunc func(context.Context, string, int) (publisher.PullRequestState, error)

func (f previousPRFunc) State(ctx context.Context, repo string, number int) (publisher.PullRequestState, error) {
	return f(ctx, repo, number)
}

// One named case per row x column cell of spec 11, 3.4. No PR mutation or Git
// operation is available through the fake; the next stage receives only a plan.
func TestCheckPreviousPRSpec3_4Table(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		state     publisher.PullRequestState
		newBranch bool
		status    model.ExecutionStatus
		class     model.ErrorClass
		cause     model.BlockCause
		reason    string
		guidance  string
	}{
		{name: "no_previous_PR/continue", mode: "continue"},
		{name: "no_previous_PR/fresh", mode: "fresh", newBranch: true},
		{name: "open_PR/continue", mode: "continue", state: publisher.PROpen},
		{name: "open_PR/fresh", mode: "fresh", state: publisher.PROpen, status: model.ExecutionBlocked, cause: model.BlockGitState, guidance: "クローズ"},
		{name: "closed_unmerged_PR/continue", mode: "continue", state: publisher.PRClosed, status: model.ExecutionFailed, class: model.ErrorConfig, reason: "pr_closed", guidance: "fresh"},
		{name: "closed_unmerged_PR/fresh", mode: "fresh", state: publisher.PRClosed, newBranch: true},
		{name: "merged_PR/continue", mode: "continue", state: publisher.PRMerged, status: model.ExecutionFailed, class: model.ErrorConfig, reason: "pr_merged", guidance: "新しいタスク"},
		{name: "merged_PR/fresh", mode: "fresh", state: publisher.PRMerged, status: model.ExecutionFailed, class: model.ErrorConfig, reason: "pr_merged", guidance: "新しいタスク"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var number *int
			if tc.state != "" {
				number = new(17)
			}
			calls := 0
			ctx := context.WithValue(context.Background(), testContextKey{}, "preparation")
			lookup := previousPRFunc(func(gotCtx context.Context, repo string, got int) (publisher.PullRequestState, error) {
				calls++
				if gotCtx != ctx || repo != "https://github.com/owner/repo" || got != 17 || number == nil {
					t.Fatal("lookup lost previous PR identity or preparation context")
				}
				return tc.state, nil
			})
			plan, err := publisher.CheckPreviousPR(ctx, "https://github.com/owner/repo", tc.mode, number, lookup)
			wantCalls := 0
			if number != nil {
				wantCalls = 1
			}
			if calls != wantCalls || publisher.ErrorClass(err) != tc.class {
				t.Fatalf("calls=%d class=%s err=%v", calls, publisher.ErrorClass(err), err)
			}
			if tc.status == "" {
				if err != nil || plan.NewBranch != tc.newBranch {
					t.Fatalf("plan=%+v err=%v; want NewBranch=%v", plan, err, tc.newBranch)
				}
				return
			}
			var stopped *publisher.RerunError
			if !errors.As(err, &stopped) {
				t.Fatalf("expected refusal, got plan=%+v err=%v", plan, err)
			}
			if plan != (publisher.RerunPlan{}) || stopped.Status != tc.status || stopped.Class != tc.class ||
				stopped.Cause != tc.cause || stopped.Reason != tc.reason || !strings.Contains(err.Error(), tc.guidance) {
				t.Fatalf("plan=%+v refusal=%+v; want status=%s class=%s cause=%s reason=%s guidance=%s",
					plan, *stopped, tc.status, tc.class, tc.cause, tc.reason, tc.guidance)
			}
			if publisher.ErrorClass(fmt.Errorf("stage: %w", err)) != tc.class {
				t.Fatal("stage wrapping lost refusal classification")
			}
		})
	}
}

type testContextKey struct{}

func TestCheckPreviousPRStopsOnUnknownStateOrLookupFailure(t *testing.T) {
	for _, mode := range []string{"continue", "fresh"} {
		for _, state := range []publisher.PullRequestState{"", "unknown", publisher.PROpen} {
			t.Run(mode+"/"+string(state), func(t *testing.T) {
				calls := 0
				lookup := previousPRFunc(func(context.Context, string, int) (publisher.PullRequestState, error) {
					calls++
					if state == publisher.PROpen {
						return state, errors.New(secret)
					}
					return state, nil
				})
				plan, err := publisher.CheckPreviousPR(context.Background(), "https://github.com/owner/repo", mode, new(17), lookup)
				if plan != (publisher.RerunPlan{}) || !errors.Is(err, publisher.ErrPullRequest) ||
					publisher.ErrorClass(err) != model.ErrorPublishFailed || calls != 1 || strings.Contains(err.Error(), secret) {
					t.Fatalf("plan=%+v calls=%d err=%v", plan, calls, err)
				}
			})
		}
	}
}

func TestCheckPreviousPRRejectsInvalidConfigurationBeforeLookup(t *testing.T) {
	lookup := previousPRFunc(func(context.Context, string, int) (publisher.PullRequestState, error) {
		t.Fatal("invalid configuration reached GitHub")
		return "", nil
	})
	for _, tc := range []struct {
		name, mode, repo string
		number           *int
		lookup           publisher.PreviousPullRequest
	}{
		{"missing_mode", "", "https://github.com/owner/repo", nil, lookup},
		{"unknown_mode", "restart", "https://github.com/owner/repo", nil, lookup},
		{"missing_repository", "continue", "", new(17), lookup},
		{"invalid_number", "fresh", "https://github.com/owner/repo", new(0), lookup},
		{"negative_number", "continue", "https://github.com/owner/repo", new(-1), lookup},
		{"missing_lookup", "fresh", "https://github.com/owner/repo", new(17), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := publisher.CheckPreviousPR(context.Background(), tc.repo, tc.mode, tc.number, tc.lookup)
			if plan != (publisher.RerunPlan{}) || !errors.Is(err, publisher.ErrRerunConfiguration) || publisher.ErrorClass(err) != model.ErrorConfig {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
		})
	}
}

func TestCheckPreviousPRHonorsCancellationBeforeAndDuringLookup(t *testing.T) {
	for _, mode := range []string{"continue", "fresh"} {
		for _, during := range []bool{false, true} {
			for _, fail := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/during=%t/fail=%t", mode, during, fail), func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if !during {
						cancel()
					}
					calls := 0
					lookup := previousPRFunc(func(context.Context, string, int) (publisher.PullRequestState, error) {
						calls++
						cancel()
						if fail {
							return "", errors.New(secret)
						}
						return publisher.PROpen, nil
					})
					plan, err := publisher.CheckPreviousPR(ctx, "https://github.com/owner/repo", mode, new(17), lookup)
					if plan != (publisher.RerunPlan{}) || !errors.Is(err, context.Canceled) || publisher.ErrorClass(err) != model.ErrorCancelled ||
						(!during && calls != 0) || (during && calls != 1) {
						t.Fatalf("plan=%+v calls=%d err=%v", plan, calls, err)
					}
				})
			}
		}
	}
}
