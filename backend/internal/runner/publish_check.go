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

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/publisher"
)

// directPushPolicy is the approval policy value that excludes PR merges from
// approval (spec 17 3.2).
const directPushPolicy = "direct_push"

var ErrPublishCheck = errors.New("runner: publish check failed")

// PublishCheck is the POST /api/executions/{id}/publish-check response
// (spec 17 3.7.1, 5章): the cancellation request and the current approval
// policy of the Execution's Project.
type PublishCheck struct {
	CancelRequested bool `json:"cancelRequested"`
	ApprovalPolicy  struct {
		Version          int    `json:"version"`
		MergePullRequest string `json:"mergePullRequest"`
	} `json:"approvalPolicy"`
}

// claimDirectPush reports whether the claim-time approval policy is
// direct_push. Any other or unreadable value is the default require_approval.
func claimDirectPush(claim Claim) bool {
	value, _ := claim.Project.ApprovalPolicy["mergePullRequest"].(string)
	return value == directPushPolicy
}

// CheckPublish calls publish-check once. It is not retried; any failure
// means the caller must not push to the Base Branch (Fail Closed).
func (r *Runner) CheckPublish(ctx context.Context, executionID string) (PublishCheck, error) {
	if parsed, err := ulid.ParseStrict(executionID); err != nil || parsed.String() != executionID {
		return PublishCheck{}, ErrPublishCheck
	}
	endpoint, err := url.JoinPath(r.controlPlaneURL, "api", "executions", executionID, "publish-check")
	if err != nil {
		return PublishCheck{}, ErrPublishCheck
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return PublishCheck{}, ErrPublishCheck
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		// Do not expose URLs or transport errors.
		return PublishCheck{}, ErrPublishCheck
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return PublishCheck{}, fmt.Errorf("runner: publish check returned HTTP %d", response.StatusCode)
	}
	var reply PublishCheck
	if json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&reply) != nil ||
		reply.ApprovalPolicy.Version != 1 || reply.ApprovalPolicy.MergePullRequest == "" {
		return PublishCheck{}, ErrPublishCheck
	}
	return reply, nil
}

// directPushCheck is the final pre-push confirmation of a direct push (spec
// 17 3.7.1 steps 1 and 2). A cancellation request cancels the Execution as a
// heartbeat would; a policy other than direct_push selects the PR path.
func (r *Runner) directPushCheck(executionID string, pushing func()) func(context.Context) error {
	return func(ctx context.Context) error {
		check, err := r.CheckPublish(ctx, executionID)
		if err != nil {
			return err
		}
		if check.CancelRequested {
			r.mu.Lock()
			cancel := r.executions[executionID]
			r.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			return context.Canceled
		}
		if check.ApprovalPolicy.MergePullRequest != directPushPolicy {
			return publisher.ErrPolicyChanged
		}
		pushing()
		return nil
	}
}
