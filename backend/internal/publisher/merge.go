package publisher

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Merge outcomes of an approved merge (spec 17 3.5.2 step 3, 6章).
var (
	// ErrMergeContentChanged: GitHub refused the merge because the PR head
	// is not the sha parameter (HTTP 409).
	ErrMergeContentChanged = errors.New("publisher: merge refused, head changed")
	// ErrMergeRejected: GitHub answered and did not merge (not mergeable,
	// forbidden, validation failure, ...).
	ErrMergeRejected = errors.New("publisher: merge refused")
	// ErrMergeUnknown: the merge request was sent but its result is not
	// known (no response, server error, unreadable answer). The caller must
	// not request the merge again with the same approval (3.5.3).
	ErrMergeUnknown = errors.New("publisher: merge result unknown")
)

// PullRequestStatus is what the Git Publisher reads just before the
// operation (spec 17 3.5.2 step 1).
type PullRequestStatus struct {
	HeadSHA    string
	BaseBranch string
	Open       bool
	Merged     bool
}

// Merger is the GitHub boundary of the approved merge.
type Merger interface {
	PullRequest(ctx context.Context, repositoryURL string, number int) (PullRequestStatus, error)
	Merge(ctx context.Context, repositoryURL string, number int, sha, method string) (string, error)
}

var _ Merger = (*GitHub)(nil)

var fullSHA = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// PullRequest reads the PR's head SHA, base branch, and open/merged state.
func (g *GitHub) PullRequest(ctx context.Context, repositoryURL string, number int) (PullRequestStatus, error) {
	repo, err := g.repository(repositoryURL)
	if err != nil {
		return PullRequestStatus{}, err
	}
	if number <= 0 {
		return PullRequestStatus{}, ErrPRConfiguration
	}
	var pr githubPR
	if err := g.request(ctx, http.MethodGet, "/repos/"+repo+"/pulls/"+strconv.Itoa(number), nil, http.StatusOK, &pr); err != nil {
		return PullRequestStatus{}, err
	}
	if pr.Number != number || pr.Merged == nil || !fullSHA.MatchString(pr.Head.SHA) || pr.Base.Ref == "" ||
		(pr.State != "open" && pr.State != "closed") ||
		!strings.EqualFold(pr.URL, "https://github.com/"+repo+"/pull/"+strconv.Itoa(number)) {
		return PullRequestStatus{}, ErrPullRequest
	}
	return PullRequestStatus{HeadSHA: pr.Head.SHA, BaseBranch: pr.Base.Ref, Open: pr.State == "open", Merged: *pr.Merged}, nil
}

// Merge sends one merge request with sha as the required PR head (spec 17
// 3.5.2 step 3, requirements 15.8) and returns the merge commit SHA. It is
// never retried here: a lost answer is ErrMergeUnknown.
func (g *GitHub) Merge(ctx context.Context, repositoryURL string, number int, sha, method string) (string, error) {
	repo, err := g.repository(repositoryURL)
	if err != nil {
		return "", err
	}
	if number <= 0 || !fullSHA.MatchString(sha) || method == "" {
		return "", ErrPRConfiguration
	}
	input := struct {
		SHA         string `json:"sha"`
		MergeMethod string `json:"merge_method"`
	}{sha, method}
	response, err := g.send(ctx, http.MethodPut, "/repos/"+repo+"/pulls/"+strconv.Itoa(number)+"/merge", input)
	var sent *transportError
	if errors.As(err, &sent) {
		// The request may have reached GitHub.
		return "", ErrMergeUnknown
	}
	if err != nil {
		// Not sent (no token, cancelled before sending): nothing merged.
		return "", err
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusOK:
	case response.StatusCode == http.StatusConflict:
		return "", ErrMergeContentChanged
	case response.StatusCode >= 400 && response.StatusCode < 500:
		return "", ErrMergeRejected
	default:
		return "", ErrMergeUnknown
	}
	var merged struct {
		SHA    string `json:"sha"`
		Merged *bool  `json:"merged"`
	}
	if decodeResponse(response, &merged) != nil || merged.Merged == nil || !*merged.Merged || !fullSHA.MatchString(merged.SHA) {
		return "", ErrMergeUnknown
	}
	return merged.SHA, nil
}
