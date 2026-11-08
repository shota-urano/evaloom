package linear_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
)

// keyCanary is a fake key; the tests assert it never leaks.
const keyCanary = "lin_api_EVALOOM_IAO3_FAKE_KEY_6f1d2c"

const teamID = "team-1"

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func writeKey(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), linear.APIKeyFileName)
	if err := os.WriteFile(path, []byte(keyCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func newClient(t *testing.T, server *httptest.Server, keyPath string, logs *bytes.Buffer) *linear.Client {
	t.Helper()
	client, err := linear.NewClient(server.Client(), server.URL+"/graphql", keyPath, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func issueJSON(n int) string {
	return fmt.Sprintf(`{"id":"issue-%d","identifier":"ENG-%d","url":"https://linear.app/x/issue/ENG-%d","title":"t%d","description":"body %d",
"updatedAt":"2026-11-07T01:02:03.000Z","archivedAt":null,"trashed":null,"team":{"id":"%s"},"state":{"type":"started"},
"labels":{"nodes":[{"name":"ai-ready"},{"name":"repo:x"}],"pageInfo":{"hasNextPage":false}}}`, n, n, n, n, n, teamID)
}

func page(ids []int, next string) string {
	nodes := make([]string, len(ids))
	for i, id := range ids {
		nodes[i] = issueJSON(id)
	}
	info := `{"hasNextPage":false,"endCursor":null}`
	if next != "" {
		info = `{"hasNextPage":true,"endCursor":"` + next + `"}`
	}
	return `{"data":{"issues":{"nodes":[` + strings.Join(nodes, ",") + `],"pageInfo":` + info + `}}}`
}

func decode(t *testing.T, r *http.Request) gqlRequest {
	t.Helper()
	var req gqlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("bad request body: %v", err)
	}
	return req
}

func assertNoKey(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if strings.Contains(text, keyCanary) {
			t.Fatalf("API key value leaked: %q", text)
		}
	}
}

func TestListReadyIssuesReadsAllPages(t *testing.T) {
	var calls atomic.Int32
	var afters []any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != keyCanary {
			t.Errorf("Authorization header is not the key from the file")
		}
		req := decode(t, r)
		if req.Variables["teamId"] != teamID || req.Variables["label"] != "ai-ready" {
			t.Errorf("unexpected variables: %v", req.Variables)
		}
		if strings.Contains(req.Query, "comments") || strings.Contains(req.Query, "attachments") {
			t.Errorf("query must not read comments or attachments (spec 15 3.2)")
		}
		afters = append(afters, req.Variables["after"])
		switch req.Variables["after"] {
		case nil:
			fmt.Fprint(w, page([]int{1, 2}, "c1"))
		case "c1":
			fmt.Fprint(w, page([]int{3, 4}, "c2"))
		case "c2":
			fmt.Fprint(w, page([]int{5}, ""))
		default:
			t.Errorf("unexpected cursor %v", req.Variables["after"])
		}
	}))
	defer server.Close()
	var logs bytes.Buffer
	issues, err := newClient(t, server, writeKey(t, 0o600), &logs).ListReadyIssues(context.Background(), teamID, "ai-ready")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || fmt.Sprint(afters) != "[<nil> c1 c2]" {
		t.Fatalf("pages not read in order: calls=%d afters=%v", calls.Load(), afters)
	}
	if len(issues) != 5 {
		t.Fatalf("got %d issues, want 5", len(issues))
	}
	for i, issue := range issues {
		want := fmt.Sprintf("issue-%d", i+1)
		if issue.ID != want || issue.Identifier != fmt.Sprintf("ENG-%d", i+1) || issue.TeamID != teamID ||
			issue.Description != fmt.Sprintf("body %d", i+1) || fmt.Sprint(issue.Labels) != "[ai-ready repo:x]" ||
			issue.Canceled() || !issue.UpdatedAt.Equal(time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC)) {
			t.Fatalf("issue %d decoded wrong: %+v", i, issue)
		}
	}
	assertNoKey(t, logs.String())
}

func TestListReadyIssuesFailsWholeListWhenAPageFails(t *testing.T) {
	cases := map[string]struct {
		respond func(w http.ResponseWriter)
		class   linear.ErrorClass
	}{
		"http 500": {func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"errors":[{"message":"boom `+keyCanary+`"}]}`)
		}, linear.ClassOther},
		"graphql error": {func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"data":null,"errors":[{"message":"echo `+keyCanary+`","extensions":{"code":"INTERNAL"}}]}`)
		}, linear.ClassOther},
		"auth": {func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"errors":[{"message":"bad key `+keyCanary+`","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`)
		}, linear.ClassAuth},
		"rate limited": {func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Requests-Reset", "1893456000000")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"errors":[{"message":"limit","extensions":{"code":"RATELIMITED"}}]}`)
		}, linear.ClassRateLimited},
		"broken json": {func(w http.ResponseWriter) { fmt.Fprint(w, `{"data":`) }, linear.ClassOther},
		"connection dropped": {func(w http.ResponseWriter) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		}, linear.ClassNetwork},
		"repeated cursor": {func(w http.ResponseWriter) { fmt.Fprint(w, page([]int{3}, "c1")) }, linear.ClassOther},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if decode(t, r).Variables["after"] == nil {
					fmt.Fprint(w, page([]int{1, 2}, "c1"))
					return
				}
				tc.respond(w)
			}))
			defer server.Close()
			var logs bytes.Buffer
			issues, err := newClient(t, server, writeKey(t, 0o600), &logs).ListReadyIssues(context.Background(), teamID, "ai-ready")
			if err == nil || issues != nil {
				t.Fatalf("partial list returned: issues=%v err=%v", issues, err)
			}
			if got := linear.ClassOf(err); got != tc.class {
				t.Fatalf("class = %q, want %q", got, tc.class)
			}
			var linearErr *linear.Error
			if !errors.As(err, &linearErr) {
				t.Fatalf("want *linear.Error, got %T", err)
			}
			if tc.class == linear.ClassRateLimited && !linearErr.ResetAt.Equal(time.UnixMilli(1893456000000)) {
				t.Fatalf("ResetAt = %v", linearErr.ResetAt)
			}
			assertNoKey(t, err.Error(), string(linear.ClassOf(err)), fmt.Sprintf("%+v", linearErr), logs.String())
		})
	}
}

func TestKeyFileProblemsStopBeforeQuery(t *testing.T) {
	cases := map[string]struct {
		path    func(t *testing.T) string
		problem linear.KeyProblem
	}{
		"missing": {func(t *testing.T) string { return filepath.Join(t.TempDir(), linear.APIKeyFileName) }, linear.KeyMissing},
		"0644":    {func(t *testing.T) string { return writeKey(t, 0o644) }, linear.KeyPermission},
		"0640":    {func(t *testing.T) string { return writeKey(t, 0o640) }, linear.KeyPermission},
		"0400":    {func(t *testing.T) string { return writeKey(t, 0o400) }, linear.KeyPermission},
		"0700":    {func(t *testing.T) string { return writeKey(t, 0o700) }, linear.KeyPermission},
		"dir":     {func(t *testing.T) string { return t.TempDir() }, linear.KeyNotRegular},
		"empty": {func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), linear.APIKeyFileName)
			if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}, linear.KeyEmpty},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
			defer server.Close()
			var logs bytes.Buffer
			client := newClient(t, server, tc.path(t), &logs)
			issues, listErr := client.ListReadyIssues(context.Background(), teamID, "ai-ready")
			_, getErr := client.Issue(context.Background(), "issue-1")
			checkErr := client.CheckKey()
			for _, err := range []error{listErr, getErr, checkErr} {
				var keyErr *linear.KeyError
				if !errors.As(err, &keyErr) || keyErr.Problem != tc.problem {
					t.Fatalf("err = %v, want KeyError %q", err, tc.problem)
				}
				assertNoKey(t, err.Error())
			}
			if issues != nil || calls.Load() != 0 {
				t.Fatalf("query started despite unusable key: calls=%d", calls.Load())
			}
			if strings.Count(logs.String(), "reason="+string(tc.problem)) != 3 {
				t.Fatalf("reason not logged for each refused query: %q", logs.String())
			}
			assertNoKey(t, logs.String(), string(linear.ClassOf(listErr)))
		})
	}
}

func TestIssueFetch(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decode(t, r)
		switch req.Variables["id"] {
		case "issue-c":
			fmt.Fprint(w, `{"data":{"issue":{"id":"issue-c","identifier":"ENG-9","url":"u","title":"t","description":null,
"updatedAt":"2026-11-07T00:00:00Z","archivedAt":"2026-11-07T00:00:01Z","trashed":true,"team":{"id":"team-2"},
"state":{"type":"canceled"},"labels":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}`)
		case "gone":
			fmt.Fprint(w, `{"data":{"issue":null}}`)
		case "many-labels":
			fmt.Fprint(w, `{"data":{"issue":{"id":"many-labels","identifier":"ENG-1","url":"u","title":"t","updatedAt":"2026-11-07T00:00:00Z",
"team":{"id":"team-1"},"state":{"type":"started"},"labels":{"nodes":[{"name":"a"}],"pageInfo":{"hasNextPage":true}}}}}`)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	var logs bytes.Buffer
	client := newClient(t, server, writeKey(t, 0o600), &logs)

	issue, err := client.Issue(context.Background(), "issue-c")
	if err != nil {
		t.Fatal(err)
	}
	if !issue.Canceled() || !issue.Archived || !issue.Trashed || issue.TeamID != "team-2" || len(issue.Labels) != 0 || issue.Description != "" {
		t.Fatalf("decoded wrong: %+v", issue)
	}
	if _, err := client.Issue(context.Background(), "gone"); !errors.Is(err, linear.ErrIssueNotFound) {
		t.Fatalf("want ErrIssueNotFound, got %v", err)
	}
	if _, err := client.Issue(context.Background(), "many-labels"); linear.ClassOf(err) != linear.ClassOther {
		t.Fatalf("incomplete label list must fail, got %v", err)
	}
	if _, err := client.Issue(context.Background(), "down"); linear.ClassOf(err) != linear.ClassOther {
		t.Fatalf("503 must fail as other, got %v", err)
	}
	assertNoKey(t, logs.String())
}

func TestRateLimitHTTP429UsesRetryAfter(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	var logs bytes.Buffer
	before := time.Now()
	_, err := newClient(t, server, writeKey(t, 0o600), &logs).Issue(context.Background(), "x")
	var linearErr *linear.Error
	if !errors.As(err, &linearErr) || linearErr.Class != linear.ClassRateLimited ||
		linearErr.ResetAt.Before(before.Add(30*time.Second)) || linearErr.ResetAt.After(time.Now().Add(30*time.Second)) {
		t.Fatalf("got %+v", err)
	}
}

func TestRedirectIsNotFollowedWithKey(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	var logs bytes.Buffer
	_, err := newClient(t, server, writeKey(t, 0o600), &logs).Issue(context.Background(), "x")
	if err == nil || hits.Load() != 0 {
		t.Fatalf("redirect followed: err=%v hits=%d", err, hits.Load())
	}
}

func TestNewClientRejectsNonHTTPS(t *testing.T) {
	for _, endpoint := range []string{"http://api.linear.app/graphql", "", "https://user:pw@api.linear.app/graphql"} {
		if _, err := linear.NewClient(nil, endpoint, "/k", nil); err == nil {
			t.Fatalf("accepted %q", endpoint)
		}
	}
	if _, err := linear.NewClient(nil, linear.Endpoint, "/k", nil); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultAPIKeyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := linear.DefaultAPIKeyPath()
	if err != nil || path != filepath.Join(home, ".config", "evaloom", "linear-api-key") {
		t.Fatalf("path=%q err=%v", path, err)
	}
}
