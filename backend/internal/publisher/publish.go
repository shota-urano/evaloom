package publisher

import (
	"context"
	"errors"
)

var (
	ErrPRConfiguration = errors.New("publisher: pull request configuration is required")
	ErrPullRequest     = errors.New("publisher: pull request operation failed")
	ErrMultiplePRs     = errors.New("publisher: multiple open pull requests for branch")
	ErrPublication     = errors.New("publisher: publication recording failed")
)

// PullRequest contains only the metadata recorded on executions (spec 11).
type PullRequest struct {
	Number int
	URL    string
}

// PullRequests is the GitHub REST boundary. FindOpen must search the repository
// for this exact head branch, across all base branches, and fail if ambiguous.
// Authentication is owned by the implementation, never by the harness/worktree.
type PullRequests interface {
	FindOpen(ctx context.Context, repositoryURL, branch string) (*PullRequest, error)
	Create(ctx context.Context, repositoryURL, head, base, title, body string) (PullRequest, error)
	Comment(ctx context.Context, repositoryURL string, number int, body string) error
}

// PublicationRecorder is the durable Control Plane boundary. RecordPushed must
// save pushed_sha and emit pushed {sha, branch} before acknowledging success.
// RecordPR must save pr_number/pr_url and, only when created is true, emit
// pr_created {prNumber, prUrl}. Implementations must not acknowledge an
// uncommitted write. The caller binds Execution identity and delivery timeout.
//
// Spec 11 requires saving pushed_sha before PR creation, but the existing result
// API only accepts state transitions (except baseline measurements). No new API
// or same-state reporting contract is invented here; that gap stays behind this
// interface. RecordPR receives the same delivery context even after cancellation.
type PublicationRecorder interface {
	RecordPushed(ctx context.Context, sha, branch string) error
	RecordPR(ctx context.Context, pr PullRequest, created bool) error
}

type PublishConfig struct {
	Push         PushConfig
	BaseBranch   string
	PullRequests PullRequests
	Recorder     PublicationRecorder
	// ReportContext survives Execution cancellation, as in Runner result delivery.
	ReportContext context.Context
	Report        Report
}

// Publication retains a confirmed push and any returned PR even on later
// failures, so the caller can include them in a failed result report. A created
// PR must be found again on continue rather than blindly replaying Create.
type Publication struct {
	PushedSHA string
	PR        *PullRequest
	Created   bool
}

// Publish layers spec 11, 3.3 over Push. It never retries external mutations,
// closes PRs or merges. Run once per Execution after all sandbox writers stop.
func Publish(ctx context.Context, target Target, config PublishConfig) (Publication, error) {
	var publication Publication
	if config.BaseBranch == "" || config.PullRequests == nil || config.Recorder == nil || config.ReportContext == nil {
		return publication, ErrPRConfiguration
	}
	body, err := config.Report.body(target.VerifiedSHA)
	if err != nil {
		return publication, err
	}
	sha, err := Push(ctx, target, config.Push)
	if err != nil {
		return publication, err
	}
	publication.PushedSHA = sha
	if err := config.Recorder.RecordPushed(config.ReportContext, sha, target.BranchName); err != nil {
		return publication, ErrPublication
	}
	if err := ctx.Err(); err != nil {
		return publication, err
	}
	pr, err := config.PullRequests.FindOpen(ctx, config.Push.RepositoryURL, target.BranchName)
	if err != nil {
		return publication, prError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return publication, err
	}
	if pr == nil {
		created, err := config.PullRequests.Create(ctx, config.Push.RepositoryURL, target.BranchName,
			config.BaseBranch, config.Report.TaskID+": "+config.Report.Title, body)
		if err != nil {
			return publication, prError(ctx, err)
		}
		pr, publication.Created = &created, true
	} else {
		if pr.Number <= 0 || pr.URL == "" {
			return publication, ErrPullRequest
		}
		publication.PR = pr
		if err := ctx.Err(); err != nil {
			return publication, err
		}
		if err := config.PullRequests.Comment(ctx, config.Push.RepositoryURL, pr.Number, body); err != nil {
			// Keep the known PR even when commenting failed.
			if recordErr := config.Recorder.RecordPR(config.ReportContext, *pr, false); recordErr != nil {
				return publication, errors.Join(prError(ctx, err), ErrPublication)
			}
			return publication, prError(ctx, err)
		}
	}
	if pr.Number <= 0 || pr.URL == "" {
		return publication, ErrPullRequest
	}
	publication.PR = pr
	if err := config.Recorder.RecordPR(config.ReportContext, *pr, publication.Created); err != nil {
		return publication, ErrPublication
	}
	return publication, ctx.Err()
}

func prError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrMultiplePRs) {
		return ErrMultiplePRs
	}
	// Do not expose transport errors, response bodies or credentials.
	return ErrPullRequest
}
