package publisher_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/publisher"
)

func TestReadSessionsIssueSeparateTokensPerStageAndReuseForPRReads(t *testing.T) {
	_, key := appKey(t)
	tokens, reads := 0, 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/app/") {
			tokens++
			var body struct {
				Permissions map[string]string `json:"permissions"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Permissions,
				map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"}) {
				t.Fatal("read session requested write permissions")
			}
			bodyJSON := strings.ReplaceAll(tokenResponse(), `"write"`, `"read"`)
			bodyJSON = strings.ReplaceAll(bodyJSON, credentialCanary, "stage-token-"+strconv.Itoa(tokens))
			return reply(201, bodyJSON), nil
		}
		reads++
		if r.Method != "GET" || r.URL.Path != "/repos/owner/repo/pulls/7" || r.Header.Get("Authorization") != "Bearer stage-token-"+strconv.Itoa(tokens) {
			t.Fatal("PR read used the wrong token or destination")
		}
		return reply(200, `{"number":7,"html_url":"https://github.com/owner/repo/pull/7","state":"open","merged":false}`), nil
	})}
	reader, err := publisher.NewGitHubAppReader(client, "https://github.com/owner/repo", func(context.Context) ([]byte, error) { return key, nil })
	if err != nil {
		t.Fatal(err)
	}
	for stage := 1; stage <= 3; stage++ {
		var excluded []string
		session, err := reader.ForStage(context.Background(), func(values ...string) { excluded = append(excluded, values...) })
		if err != nil {
			t.Fatal(err)
		}
		want := "stage-token-" + strconv.Itoa(stage)
		if !reflect.DeepEqual(excluded, []string{want, base64.StdEncoding.EncodeToString([]byte("x-access-token:" + want))}) {
			t.Fatal("read token was not registered for log exclusion")
		}
		for range 2 {
			state, err := session.State(context.Background(), "git@github.com:owner/repo.git", 7)
			if err != nil || state != publisher.PROpen {
				t.Fatal("read session PR lookup failed")
			}
		}
		if _, err := session.State(context.Background(), "https://github.com/other/repo", 7); !errors.Is(err, publisher.ErrPRConfiguration) {
			t.Fatal("read session accepted another repository")
		}
	}
	if tokens != 3 || reads != 6 {
		t.Fatalf("token requests=%d PR reads=%d", tokens, reads)
	}
	session, err := reader.ForStage(context.Background(), func(...string) {})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"push", "--no-tags", "--refmap=", "--", "https://github.com/owner/repo", "main"},
		{"fetch", "--no-tags", "--refmap=", "--", "https://github.com/other/repo", "+refs/heads/*:refs/remotes/origin/*"},
		{"clone", "--recurse-submodules", "--no-local", "--origin", "origin", "--", "https://github.com/owner/repo", t.TempDir()},
		{"ls-remote", "--heads", "--refs", "--", "https://github.com/owner/repo", "--upload-pack=malicious"},
	} {
		_, err := session.Output(context.Background(), t.TempDir(), args...)
		if err == nil || strings.Contains(err.Error(), "stage-token") {
			t.Fatal("read session accepted a write/unsafe Git operation or exposed credentials")
		}
	}
	if tokens != 4 || reads != 6 {
		t.Fatal("rejected Git command requested additional credentials")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.ForStage(ctx, func(...string) {}); !errors.Is(err, context.Canceled) || tokens != 4 {
		t.Fatal("cancelled stage issued a token")
	}
	if _, err := reader.ForStage(context.Background(), nil); err == nil || tokens != 4 {
		t.Fatal("stage without log exclusion issued a token")
	}
}
