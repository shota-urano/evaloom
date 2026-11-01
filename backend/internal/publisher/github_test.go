package publisher_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/publisher"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const prJSON = `{"number":17,"html_url":"https://github.com/owner/repo/pull/17","state":"open","head":{"ref":"evaloom/task-1-short","repo":{"full_name":"owner/repo"}},"base":{"ref":"main"}}`

func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func githubClient(t *testing.T, transport roundTripFunc) *publisher.GitHub {
	t.Helper()
	client, err := publisher.NewGitHub(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Fatal("unexpected GitHub host or authentication headers")
		}
		return transport(r)
	})}, func(context.Context) (string, error) { return secret, nil })
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestGitHubRESTCreateLookupAndComment(t *testing.T) {
	var calls []string
	client := githubClient(t, func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet {
			q := r.URL.Query()
			if q.Get("state") != "open" || q.Get("head") != "owner:evaloom/task-1-short" || q.Get("per_page") != "2" || q.Has("base") {
				t.Fatalf("query=%s", r.URL.RawQuery)
			}
			if len(calls) == 1 {
				return reply(http.StatusOK, `[]`), nil
			}
			return reply(http.StatusOK, `[`+prJSON+`]`), nil
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatal("missing Content-Type")
		}
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if r.URL.Path == "/repos/owner/repo/pulls" {
			want := map[string]string{"head": "evaloom/task-1-short", "base": "main", "title": "task-1: title", "body": "execution result\nsecond line"}
			if !reflect.DeepEqual(input, want) {
				t.Fatalf("create=%v", input)
			}
			return reply(http.StatusCreated, prJSON), nil
		}
		if !reflect.DeepEqual(input, map[string]string{"body": "next execution\nresult"}) {
			t.Fatalf("comment=%v", input)
		}
		return reply(http.StatusCreated, `{"id":123}`), nil
	})
	ctx := context.Background()
	pr, err := client.FindOpen(ctx, "https://github.com/owner/repo.git", "evaloom/task-1-short")
	if err != nil || pr != nil {
		t.Fatalf("pr=%v err=%v", pr, err)
	}
	created, err := client.Create(ctx, "git@github.com:owner/repo.git", "evaloom/task-1-short", "main", "task-1: title", "execution result\nsecond line")
	if err != nil || created.Number != 17 || created.URL != "https://github.com/owner/repo/pull/17" {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	pr, err = client.FindOpen(ctx, "ssh://git@github.com/owner/repo.git", "evaloom/task-1-short")
	if err != nil || pr == nil || *pr != created {
		t.Fatalf("pr=%v err=%v", pr, err)
	}
	if err := client.Comment(ctx, "https://github.com/owner/repo", pr.Number, "next execution\nresult"); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{"GET /repos/owner/repo/pulls", "POST /repos/owner/repo/pulls", "GET /repos/owner/repo/pulls", "POST /repos/owner/repo/issues/17/comments"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls=%v", calls)
	}
}

func TestGitHubLookupFailsClosedOnAmbiguousOrInvalidResponse(t *testing.T) {
	for name, body := range map[string]string{
		"multiple":         `[` + prJSON + `,` + prJSON + `]`,
		"null":             `null`,
		"malformed":        `[{`,
		"trailing":         `[] {}`,
		"missing_metadata": `[{}]`,
		"closed":           `[` + strings.Replace(prJSON, `"state":"open"`, `"state":"closed"`, 1) + `]`,
		"other_head":       `[` + strings.Replace(prJSON, `"ref":"evaloom/task-1-short"`, `"ref":"evaloom/other"`, 1) + `]`,
		"fork":             `[` + strings.Replace(prJSON, `"full_name":"owner/repo"`, `"full_name":"fork/repo"`, 1) + `]`,
		"bad_url":          `[` + strings.Replace(prJSON, "https://github.com/owner/repo/pull/17", "https://other.invalid/?token="+secret, 1) + `]`,
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			client := githubClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet {
					t.Fatal("invalid lookup triggered mutation")
				}
				return reply(http.StatusOK, body), nil
			})
			pr, err := client.FindOpen(context.Background(), "https://github.com/owner/repo", "evaloom/task-1-short")
			if err == nil || pr != nil || calls != 1 || strings.Contains(err.Error(), secret) {
				t.Fatalf("pr=%v calls=%d err=%v", pr, calls, err)
			}
			if name == "multiple" && !errors.Is(err, publisher.ErrMultiplePRs) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestGitHubDoesNotRetryFailedMutationsOrExposeResponses(t *testing.T) {
	for _, status := range []int{301, 401, 403, 404, 422, 500} {
		for _, operation := range []string{"find", "create", "comment", "state"} {
			t.Run(operation+"/"+http.StatusText(status), func(t *testing.T) {
				calls := 0
				client := githubClient(t, func(*http.Request) (*http.Response, error) {
					calls++
					r := reply(status, secret)
					r.Header.Set("Location", "https://api.github.com/redirect?token="+secret)
					return r, nil
				})
				var err error
				switch operation {
				case "state":
					_, err = client.State(context.Background(), "https://github.com/owner/repo", 17)
				case "find":
					_, err = client.FindOpen(context.Background(), "https://github.com/owner/repo", "evaloom/task-1-short")
				case "create":
					_, err = client.Create(context.Background(), "https://github.com/owner/repo", "evaloom/task-1-short", "main", "title", "body")
				case "comment":
					err = client.Comment(context.Background(), "https://github.com/owner/repo", 17, "body")
				}
				if !errors.Is(err, publisher.ErrPullRequest) || calls != 1 || strings.Contains(err.Error(), secret) {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			})
		}
	}
}

func TestGitHubPreviousPRState(t *testing.T) {
	for _, tc := range []struct {
		name, state, merged string
		want                publisher.PullRequestState
	}{
		{"open", "open", "false", publisher.PROpen},
		{"closed_unmerged", "closed", "false", publisher.PRClosed},
		{"merged", "closed", "true", publisher.PRMerged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := githubClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo/pulls/17" || r.URL.RawQuery != "" || r.Body != nil {
					t.Fatalf("request=%s %s", r.Method, r.URL)
				}
				body := strings.Replace(prJSON, `"state":"open"`, `"state":"`+tc.state+`","merged":`+tc.merged, 1)
				return reply(http.StatusOK, body), nil
			})
			state, err := client.State(context.Background(), "git@github.com:owner/repo.git", 17)
			if err != nil || state != tc.want || calls != 1 {
				t.Fatalf("state=%q calls=%d err=%v", state, calls, err)
			}
		})
	}
}

func TestGitHubPreviousPRStateRejectsInvalidResponses(t *testing.T) {
	valid := strings.Replace(prJSON, `"state":"open"`, `"state":"closed","merged":false`, 1)
	for name, body := range map[string]string{
		"missing_merge_state": prJSON,
		"null_merge_state":    strings.Replace(valid, `"merged":false`, `"merged":null`, 1),
		"invalid_merge_state": strings.Replace(valid, `"merged":false`, `"merged":"false"`, 1),
		"wrong_number":        strings.Replace(valid, `"number":17`, `"number":18`, 1),
		"wrong_url":           strings.Replace(valid, "https://github.com/owner/repo/pull/17", "https://other.invalid/"+secret, 1),
		"wrong_state":         strings.Replace(valid, `"state":"closed"`, `"state":"unknown"`, 1),
		"missing_state":       strings.Replace(valid, `"state":"closed",`, "", 1),
		"open_but_merged":     strings.Replace(strings.Replace(valid, `"state":"closed"`, `"state":"open"`, 1), `"merged":false`, `"merged":true`, 1),
		"null":                `null`,
		"empty":               `{}`,
		"malformed":           `{`,
		"trailing":            valid + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := githubClient(t, func(*http.Request) (*http.Response, error) { return reply(http.StatusOK, body), nil })
			state, err := client.State(context.Background(), "https://github.com/owner/repo", 17)
			if state != "" || !errors.Is(err, publisher.ErrPullRequest) || strings.Contains(err.Error(), secret) {
				t.Fatalf("state=%q err=%v", state, err)
			}
		})
	}
}

func TestGitHubPreviousPRStateRejectsInvalidInputsBeforeAuthentication(t *testing.T) {
	client, err := publisher.NewGitHub(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid input reached GitHub")
		return nil, errors.New("unexpected")
	})}, func(context.Context) (string, error) {
		t.Fatal("invalid input requested an installation token")
		return "", errors.New("unexpected")
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, number := range []int{0, -1} {
		if _, err := client.State(context.Background(), "https://github.com/owner/repo", number); !errors.Is(err, publisher.ErrRerunConfiguration) {
			t.Fatalf("err=%v", err)
		}
	}
	if _, err := client.State(context.Background(), "https://other.invalid/owner/repo", 17); !errors.Is(err, publisher.ErrPRConfiguration) {
		t.Fatalf("err=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.State(ctx, "https://github.com/owner/repo", 17); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestGitHubValidatesCreatedPRResponse(t *testing.T) {
	for _, body := range []string{`{}`, `null`, prJSON + `{}`, strings.Replace(prJSON, `"ref":"main"`, `"ref":"other"`, 1), strings.Replace(prJSON, `"number":17`, `"number":0`, 1)} {
		client := githubClient(t, func(*http.Request) (*http.Response, error) { return reply(http.StatusCreated, body), nil })
		pr, err := client.Create(context.Background(), "https://github.com/owner/repo", "evaloom/task-1-short", "main", "title", "body")
		if !errors.Is(err, publisher.ErrPullRequest) || pr.Number != 0 {
			t.Fatalf("pr=%+v err=%v", pr, err)
		}
	}
}

func TestGitHubDoesNotSendTokenForInvalidRepositoryOrCancelledContext(t *testing.T) {
	calls := 0
	client, err := publisher.NewGitHub(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected HTTP request")
		return nil, errors.New("unexpected")
	})}, func(context.Context) (string, error) { calls++; return secret, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"https://other.invalid/owner/repo", "https://github.com/owner/repo/extra", "https://github.com/owner/..", "https://user:pass@github.com/owner/repo", "https://github.com/owner/repo?token=" + secret} {
		if _, err := client.FindOpen(context.Background(), repo, "evaloom/task"); !errors.Is(err, publisher.ErrPRConfiguration) {
			t.Fatalf("err=%v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.FindOpen(ctx, "https://github.com/owner/repo", "evaloom/task"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if calls != 0 {
		t.Fatalf("token requests=%d", calls)
	}
}

func TestGitHubSanitizesTokenAndTransportErrors(t *testing.T) {
	for _, stage := range []string{"token", "transport"} {
		client, err := publisher.NewGitHub(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New(secret)
		})}, func(context.Context) (string, error) {
			if stage == "token" {
				return "", errors.New(secret)
			}
			return secret, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.FindOpen(context.Background(), "https://github.com/owner/repo", "evaloom/task")
		if !errors.Is(err, publisher.ErrPullRequest) || strings.Contains(err.Error(), secret) {
			t.Fatalf("err=%v", err)
		}
	}
}
