package publisher_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/publisher"
)

const mergeRepo = "https://github.com/owner/repo"

// Spec 17 3.5.2 step 3 and 6章: one PUT with sha, mapped to a definite
// refusal, a head mismatch, or an unknown result (3.5.3) without retrying.
func TestGitHubMergeSendsShaOnceAndClassifiesAnswers(t *testing.T) {
	sha := strings.Repeat("a", 40)
	merged := strings.Repeat("c", 40)
	for _, tc := range []struct {
		name   string
		answer func() (*http.Response, error)
		want   error
	}{
		{"merged", func() (*http.Response, error) { return reply(200, `{"sha":"`+merged+`","merged":true}`), nil }, nil},
		{"head changed", func() (*http.Response, error) { return reply(409, `{}`), nil }, publisher.ErrMergeContentChanged},
		{"not mergeable", func() (*http.Response, error) { return reply(405, `{}`), nil }, publisher.ErrMergeRejected},
		{"server error", func() (*http.Response, error) { return reply(502, ``), nil }, publisher.ErrMergeUnknown},
		{"no answer", func() (*http.Response, error) { return nil, errors.New("reset") }, publisher.ErrMergeUnknown},
		{"unreadable", func() (*http.Response, error) { return reply(200, `{"merged":true}`), nil }, publisher.ErrMergeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := githubClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				var input map[string]string
				if r.Method != http.MethodPut || r.URL.Path != "/repos/owner/repo/pulls/7/merge" ||
					json.NewDecoder(r.Body).Decode(&input) != nil || input["sha"] != sha || input["merge_method"] != "merge" {
					t.Fatalf("request %s %s %v", r.Method, r.URL.Path, input)
				}
				return tc.answer()
			})
			got, err := client.Merge(context.Background(), mergeRepo, 7, sha, "merge")
			if !errors.Is(err, tc.want) || calls != 1 || (tc.want == nil && got != merged) {
				t.Fatalf("merge = %q %v calls=%d; want %v", got, err, calls, tc.want)
			}
		})
	}
}

func TestGitHubPullRequestReadsHeadBaseAndState(t *testing.T) {
	head := strings.Repeat("b", 40)
	client := githubClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo/pulls/7" {
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		return reply(200, `{"number":7,"html_url":"https://github.com/owner/repo/pull/7","state":"closed","merged":true,"head":{"sha":"`+head+`"},"base":{"ref":"main"}}`), nil
	})
	got, err := client.PullRequest(context.Background(), mergeRepo, 7)
	if err != nil || got != (publisher.PullRequestStatus{HeadSHA: head, BaseBranch: "main", Open: false, Merged: true}) {
		t.Fatalf("state = %+v %v", got, err)
	}
}
