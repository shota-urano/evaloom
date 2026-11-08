package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
)

// approvalCheckInterval is how often the Runner reads the PRs waiting for
// approval (spec 17 3.6, Q58). The spec gives no value; see
// internal/api/approvals.md.
const approvalCheckInterval = time.Minute

// pullRequestClosedReason matches the Control Plane's failure reason for a PR
// closed on GitHub while it waited for approval (Q58).
const pullRequestClosedReason = "pull_request_closed"

// WaitingApproval is a PR waiting for approval, listed in the heartbeat
// response (spec 17 3.6).
type WaitingApproval struct {
	ExecutionID string          `json:"executionId"`
	TaskID      string          `json:"taskId"`
	Target      json.RawMessage `json:"target"`
	Project     model.Project   `json:"project"`
}

// PullRequestReader reads a PR's current state on GitHub.
type PullRequestReader interface {
	PullRequest(ctx context.Context, repositoryURL string, number int) (publisher.PullRequestStatus, error)
}

// approvalCheck is the periodic check of PRs waiting for approval.
type approvalCheck struct {
	reader   func(model.Project) (PullRequestReader, error)
	interval time.Duration
	now      func() time.Time
	last     time.Time
	running  bool
	waiting  []WaitingApproval
	// err is the last check's error, handed to onError on the loop goroutine.
	err error
}

// EnableApprovalCheck makes Run read the PRs waiting for approval every
// approvalCheckInterval with read-only GitHub App credentials of each Project
// (spec 17 3.6). Without it Run does not check them.
func (r *Runner) EnableApprovalCheck(config Config) {
	r.setApprovalCheck(func(project model.Project) (PullRequestReader, error) {
		reader, err := publisher.NewGitHubAppReader(r.client, project.GitRemoteURL, func(ctx context.Context) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return os.ReadFile(config.GitHubAppPrivateKeyFile)
		})
		if err != nil {
			return nil, err
		}
		return reader, nil
	}, approvalCheckInterval, time.Now)
}

func (r *Runner) setApprovalCheck(reader func(model.Project) (PullRequestReader, error), interval time.Duration, now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.approvals = approvalCheck{reader: reader, interval: interval, now: now}
}

// setWaitingApprovals keeps the latest list from the heartbeat.
func (r *Runner) setWaitingApprovals(list []WaitingApproval) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.approvals.waiting = list
}

// startApprovalCheck runs one check in the background when it is enabled,
// due, and not already running. Run calls it after each heartbeat; the error
// of the previous check is passed to onError here, on the loop goroutine.
func (r *Runner) startApprovalCheck(ctx context.Context, workers *sync.WaitGroup, onError func(error)) {
	r.mu.Lock()
	check := &r.approvals
	if previous := check.err; previous != nil {
		check.err = nil
		r.mu.Unlock()
		if onError != nil {
			onError(previous)
		}
		r.mu.Lock()
	}
	if check.reader == nil || check.running {
		r.mu.Unlock()
		return
	}
	now := check.now()
	if !check.last.IsZero() && now.Sub(check.last) < check.interval {
		r.mu.Unlock()
		return
	}
	check.last, check.running = now, true
	r.mu.Unlock()
	workers.Add(1)
	go func() {
		defer workers.Done()
		err := r.CheckWaitingApprovals(ctx)
		r.mu.Lock()
		r.approvals.running = false
		if ctx.Err() == nil {
			r.approvals.err = err
		}
		r.mu.Unlock()
	}()
}

// CheckWaitingApprovals is one pass of spec 17 3.6 (Q58) over the PRs the
// last heartbeat listed: a PR merged directly on GitHub is reported as
// succeeded with its merge commit, a PR closed without merging as failed
// (pull_request_closed). The Control Plane then invalidates the approval
// (target_changed). An open PR, and a PR that cannot be read, is left as is
// and read again at the next check. Executions this Runner is handling are
// skipped.
func (r *Runner) CheckWaitingApprovals(ctx context.Context) error {
	r.mu.Lock()
	list := append([]WaitingApproval(nil), r.approvals.waiting...)
	readerFor := r.approvals.reader
	r.mu.Unlock()
	if readerFor == nil {
		return nil
	}
	var errs []error
	for _, item := range list {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.mu.Lock()
		_, busy := r.executions[item.ExecutionID]
		r.mu.Unlock()
		if busy {
			continue
		}
		if err := r.checkWaitingApproval(ctx, item, readerFor); err != nil {
			errs = append(errs, fmt.Errorf("runner: check PR waiting for approval of %s: %w", item.ExecutionID, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Runner) checkWaitingApproval(ctx context.Context, item WaitingApproval, readerFor func(model.Project) (PullRequestReader, error)) error {
	var target mergeTarget
	if json.Unmarshal(item.Target, &target) != nil || target.PRNumber < 1 || target.Repository == "" {
		return ErrOperationInput
	}
	reader, err := readerFor(item.Project)
	if err != nil {
		return ErrOperationInput
	}
	// The reader is bound to the Project's repository and refuses any other.
	state, err := reader.PullRequest(ctx, "https://github.com/"+target.Repository, target.PRNumber)
	if err != nil {
		return err
	}
	switch {
	case state.Merged:
		if !operationSHA.MatchString(state.MergeCommitSHA) {
			return publisher.ErrPullRequest
		}
		return r.ReportResult(ctx, item.ExecutionID, ExecutionResult{FromStatus: model.ExecutionWaitingApproval,
			Status: model.ExecutionSucceeded, MergeCommitSHA: state.MergeCommitSHA})
	case !state.Open:
		return r.ReportResult(ctx, item.ExecutionID, ExecutionResult{FromStatus: model.ExecutionWaitingApproval,
			Status: model.ExecutionFailed, ErrorMessage: pullRequestClosedReason})
	}
	return nil
}
