package publisher

import (
	"context"
	"errors"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// PullRequestState distinguishes closed, unmerged PRs from merged PRs. GitHub
// reports both as state=closed; implementations must inspect merge status too.
type PullRequestState string

const (
	PROpen   PullRequestState = "open"
	PRClosed PullRequestState = "closed"
	PRMerged PullRequestState = "merged"
)

// PreviousPullRequest is the read-only preparation boundary (spec 11, 3.4).
// A known PR number must resolve to a state or an error, never to "no PR".
// It is separate from PullRequests so publishing cannot stand in for this check.
type PreviousPullRequest interface {
	State(ctx context.Context, repositoryURL string, number int) (PullRequestState, error)
}

var ErrRerunConfiguration = errors.New("publisher: rerun mode and valid previous PR inputs are required")

// RerunPlan tells the worktree stage whether to allocate a new branch. A false
// NewBranch retains the task branch (or creates its initial branch on first run).
// Branch naming/allocation belongs to spec 06; this check has no Git mutations.
type RerunPlan struct {
	NewBranch bool
}

// RerunError is an expected refusal from the table in spec 11, 3.4. The caller
// reports Status, Class and Cause using the existing result API, then stops.
// Reason is pr_closed/pr_merged where specified; no new reason code is invented
// for the open/fresh Block. Error() contains only safe, actionable diagnostics.
type RerunError struct {
	Status model.ExecutionStatus
	Class  model.ErrorClass
	Cause  model.BlockCause
	Reason string
}

func (err *RerunError) Error() string {
	switch err.Reason {
	case "pr_closed":
		return "publisher: pr_closed。前回のPRはクローズ済みです。最初からやり直す場合はfreshを選んでください"
	case "pr_merged":
		return "publisher: pr_merged。前回のPRはマージ済みです。新しいタスクを作成してください"
	default:
		return "publisher: 前回のPRが開いています。本人がPRをクローズしてからfreshで再実行してください"
	}
}

// CheckPreviousPR runs during preparation, before worktree/setup/harness work.
// previousNumber=nil means no PR has been recorded, not a failed GitHub lookup.
// The caller must supply the previous PR even in fresh mode; a fresh claim's
// previous=null alone cannot establish absence (see README's integration gap).
// Any error forbids proceeding. Unexpected lookup/authentication failures are
// publish_failed via ErrorClass; cancellation/deadlines retain their classes.
func CheckPreviousPR(ctx context.Context, repositoryURL, mode string, previousNumber *int, previous PreviousPullRequest) (RerunPlan, error) {
	if err := ctx.Err(); err != nil {
		return RerunPlan{}, err
	}
	if mode != "continue" && mode != "fresh" {
		return RerunPlan{}, ErrRerunConfiguration
	}
	if previousNumber != nil {
		if *previousNumber <= 0 || repositoryURL == "" || previous == nil {
			return RerunPlan{}, ErrRerunConfiguration
		}
		state, err := previous.State(ctx, repositoryURL, *previousNumber)
		if err != nil {
			return RerunPlan{}, prError(ctx, err)
		}
		if err := ctx.Err(); err != nil {
			return RerunPlan{}, err
		}
		switch state {
		case PROpen:
			if mode == "fresh" {
				return RerunPlan{}, &RerunError{Status: model.ExecutionBlocked, Cause: model.BlockGitState}
			}
		case PRClosed:
			if mode == "continue" {
				return RerunPlan{}, &RerunError{Status: model.ExecutionFailed, Class: model.ErrorConfig, Reason: "pr_closed"}
			}
		case PRMerged:
			return RerunPlan{}, &RerunError{Status: model.ExecutionFailed, Class: model.ErrorConfig, Reason: "pr_merged"}
		default:
			return RerunPlan{}, ErrPullRequest
		}
	}
	return RerunPlan{NewBranch: mode == "fresh"}, nil
}
