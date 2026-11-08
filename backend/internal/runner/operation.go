package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
)

// ClaimOperation is the approved operation of a claim (spec 17 3.5.1).
type ClaimOperation struct {
	ApprovalID     string          `json:"approvalId"`
	Operation      string          `json:"operation"`
	Target         json.RawMessage `json:"target"`
	ContentVersion string          `json:"contentVersion"`
}

// mergeTarget is approvals.target of merge_pull_request (spec 17 3.4).
type mergeTarget struct {
	Repository string `json:"repository"`
	PRNumber   int    `json:"prNumber"`
	BaseBranch string `json:"baseBranch"`
	HeadBranch string `json:"headBranch"`
}

// Merge failure reasons reported with publish_failed. The Control Plane
// accepts exactly these values from a merging Execution.
const (
	mergeFailureContentChanged = "content_changed"
	mergeFailureRejected       = "merge_rejected"
	mergeFailureUnknown        = "merge_unknown"
)

var (
	ErrOperationInput = errors.New("runner: invalid approved operation")
	ErrConsume        = errors.New("runner: approval consume failed")
)

var operationSHA = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// ExecuteOperation performs an approved operation with the Git Publisher's
// GitHub App credentials for the claim's Project (spec 17 3.5.1 step 3).
func (r *Runner) ExecuteOperation(ctx, reportCtx context.Context, config Config, claim Claim) error {
	github, err := publisher.NewGitHubApp(r.client, claim.Project.GitRemoteURL, func(ctx context.Context) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return os.ReadFile(config.GitHubAppPrivateKeyFile)
	})
	if err != nil {
		return ErrOperationInput
	}
	return r.ExecuteMerge(ctx, reportCtx, claim, github)
}

// ExecuteMerge is spec 17 3.5.2: read the PR, consume the approval with what
// was read, and only when consume allows it send one merge request whose sha
// parameter is content_version. A refused consume sends nothing to GitHub and
// reports nothing: consume already moved the Execution. The merge is never
// requested twice; a lost answer is reported as merge_unknown.
func (r *Runner) ExecuteMerge(ctx, reportCtx context.Context, claim Claim, github publisher.Merger) error {
	operation := claim.Operation
	if operation == nil || operation.Operation != "merge_pull_request" || !operationSHA.MatchString(operation.ContentVersion) {
		return ErrOperationInput
	}
	var target mergeTarget
	if json.Unmarshal(operation.Target, &target) != nil || target.PRNumber < 1 || target.Repository == "" || target.BaseBranch == "" {
		return ErrOperationInput
	}
	// The GitHub client is bound to the Project's repository and refuses
	// any other (spec 11 3.6).
	repository := "https://github.com/" + target.Repository
	state, err := github.PullRequest(ctx, repository, target.PRNumber)
	if err != nil {
		return fmt.Errorf("runner: read pull request before merge: %w", err)
	}
	consumed, err := r.consumeApproval(ctx, operation.ApprovalID, state)
	if err != nil {
		return err
	}
	if !consumed.Allowed {
		return nil
	}
	report := ExecutionResult{FromStatus: model.ExecutionMerging, Status: model.ExecutionFailed, ErrorClass: model.ErrorPublishFailed}
	if consumed.ContentVersion != operation.ContentVersion || consumed.MergeMethod == "" {
		// Fail closed: merge only the approved content.
		report.ErrorMessage = mergeFailureRejected
		return r.ReportResult(reportCtx, claim.Execution.ID, report)
	}
	sha, err := github.Merge(ctx, repository, target.PRNumber, consumed.ContentVersion, consumed.MergeMethod)
	switch {
	case err == nil:
		report = ExecutionResult{FromStatus: model.ExecutionMerging, Status: model.ExecutionSucceeded, MergeCommitSHA: sha}
	case errors.Is(err, publisher.ErrMergeContentChanged):
		report.ErrorMessage = mergeFailureContentChanged
	case errors.Is(err, publisher.ErrMergeUnknown):
		// Spec 17 3.5.3: do not request the merge again; read the PR again.
		if sha, merged := reconcileMerge(ctx, github, repository, target.PRNumber, consumed.ContentVersion); merged {
			report = ExecutionResult{FromStatus: model.ExecutionMerging, Status: model.ExecutionSucceeded, MergeCommitSHA: sha}
		} else {
			report.ErrorMessage = mergeFailureUnknown
		}
	default:
		report.ErrorMessage = mergeFailureRejected
	}
	return r.ReportResult(reportCtx, claim.Execution.ID, report)
}

// reconcileMerge is spec 17 3.5.3: after a merge request whose result is
// unknown, read the PR once more (never request the merge again). Only a PR
// that is merged with the approved head (content_version) and a readable
// merge commit counts as merged. An open PR, an unreadable PR, and every
// other state are reported as merge_unknown so that the owner checks GitHub.
func reconcileMerge(ctx context.Context, github publisher.Merger, repository string, number int, contentVersion string) (string, bool) {
	state, err := github.PullRequest(ctx, repository, number)
	if err != nil || !state.Merged || state.HeadSHA != contentVersion || !operationSHA.MatchString(state.MergeCommitSHA) {
		return "", false
	}
	return state.MergeCommitSHA, true
}

type consumeResponse struct {
	Allowed        bool   `json:"allowed"`
	ContentVersion string `json:"contentVersion"`
	MergeMethod    string `json:"mergeMethod"`
	Reason         string `json:"reason"`
}

// consumeApproval calls POST /api/approvals/{id}/consume once. It is not
// retried: an unanswered consume may have used the approval, and only an
// explicit allowed=true permits the merge.
func (r *Runner) consumeApproval(ctx context.Context, id string, state publisher.PullRequestStatus) (consumeResponse, error) {
	if parsed, err := ulid.ParseStrict(id); err != nil || parsed.String() != id {
		return consumeResponse{}, ErrOperationInput
	}
	endpoint, err := url.JoinPath(r.controlPlaneURL, "api", "approvals", id, "consume")
	if err != nil {
		return consumeResponse{}, ErrOperationInput
	}
	body, err := json.Marshal(map[string]any{"headSha": state.HeadSHA, "baseBranch": state.BaseBranch,
		"open": state.Open, "merged": state.Merged})
	if err != nil {
		return consumeResponse{}, ErrConsume
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return consumeResponse{}, ErrConsume
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return consumeResponse{}, ErrConsume
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return consumeResponse{}, fmt.Errorf("runner: consume returned HTTP %d", response.StatusCode)
	}
	var reply consumeResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<16))
	if decoder.Decode(&reply) != nil {
		return consumeResponse{}, ErrConsume
	}
	return reply, nil
}
