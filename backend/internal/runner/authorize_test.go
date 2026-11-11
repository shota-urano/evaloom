package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/contract"
)

func TestAuthorizeSendsRunnerCredentialsAndReturnsDecision(t *testing.T) {
	for _, reply := range []string{
		`{"allowed":true}`, `{"allowed":false,"reason":"cancelled"}`,
		`{"allowed":false,"reason":"content_changed"}`, `{"allowed":false,"reason":"not_approved"}`,
		`{"allowed":false,"reason":"already_used"}`, `{"allowed":false,"reason":"expired"}`,
	} {
		t.Run(reply, func(t *testing.T) {
			executionID, approvalID := ulid.Make().String(), ulid.Make().String()
			input := contract.AuthorizeRequest{Operation: "merge_pull_request", ApprovalID: &approvalID, ContentID: "opaque:r1"}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/control/api/executions/"+executionID+"/authorize" || r.Method != "POST" ||
					r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("Content-Type") != "application/json" ||
					r.Header.Get("Origin") != "" || len(r.Cookies()) != 0 {
					t.Errorf("unexpected request method, path or credentials")
				}
				var got contract.AuthorizeRequest
				if json.NewDecoder(r.Body).Decode(&got) != nil || got.Operation != input.Operation || got.ContentID != input.ContentID || got.ApprovalID == nil || *got.ApprovalID != approvalID {
					t.Errorf("request=%+v", got)
				}
				io.WriteString(w, reply)
			}))
			defer server.Close()
			process, err := New(testConfig(t, server.URL+"/control/"))
			if err != nil {
				t.Fatal(err)
			}
			var want contract.AuthorizeResponse
			if err := json.Unmarshal([]byte(reply), &want); err != nil {
				t.Fatal(err)
			}
			if got := process.Authorize(context.Background(), executionID, input); got != want || calls.Load() != 1 {
				t.Fatalf("reply=%+v want=%+v calls=%d", got, want, calls.Load())
			}
		})
	}
}

func TestAuthorizeFailsClosedWithoutRetry(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthenticated", 401, `{"allowed":true}`}, {"forbidden", 403, `{"allowed":true}`},
		{"conflict", 409, `{"allowed":true}`}, {"storage failure", 500, `{"allowed":false,"error":{"code":"internal"}}`},
		{"unavailable", 503, `{"allowed":true}`}, {"empty", 200, ``},
		{"no decision", 200, `{}`}, {"null decision", 200, `{"allowed":null}`},
		{"invalid type", 200, `{"allowed":"true"}`}, {"unknown field", 200, `{"allowed":true,"extra":true}`},
		{"wrong case", 200, `{"Allowed":true}`}, {"duplicate decision", 200, `{"allowed":false,"allowed":true}`},
		{"denial without reason", 200, `{"allowed":false}`}, {"unknown reason", 200, `{"allowed":false,"reason":"oops"}`},
		{"contradictory reason", 200, `{"allowed":true,"reason":"cancelled"}`},
		{"trailing JSON", 200, `{"allowed":true} {}`}, {"truncated", 200, `{"allowed":true`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			process, err := New(testConfig(t, server.URL))
			if err != nil {
				t.Fatal(err)
			}
			got := process.Authorize(context.Background(), ulid.Make().String(), contract.AuthorizeRequest{Operation: "publish", ContentID: "r1"})
			if got.Allowed || got.Reason != "unavailable" || calls.Load() != 1 {
				t.Fatalf("reply=%+v calls=%d", got, calls.Load())
			}
		})
	}
}

func TestAuthorizeUnreachableTimeoutAndRedirectReturnUnavailable(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		process, err := New(testConfig(t, server.URL))
		if err != nil {
			t.Fatal(err)
		}
		server.Close()
		got := process.Authorize(context.Background(), ulid.Make().String(), contract.AuthorizeRequest{Operation: "publish", ContentID: "r1"})
		if got.Allowed || got.Reason != "unavailable" {
			t.Fatalf("reply=%+v", got)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(200)
			io.WriteString(w, `{"allowed":true}`)
			w.(http.Flusher).Flush()
			<-r.Context().Done() // incomplete response must never authorize
		}))
		defer server.Close()
		process, err := New(testConfig(t, server.URL))
		if err != nil {
			t.Fatal(err)
		}
		process.client.Timeout = 50 * time.Millisecond
		got := process.Authorize(context.Background(), ulid.Make().String(), contract.AuthorizeRequest{Operation: "publish", ContentID: "r1"})
		if got.Allowed || got.Reason != "unavailable" || calls.Load() != 1 {
			t.Fatalf("reply=%+v calls=%d", got, calls.Load())
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var forwarded atomic.Int32
		destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forwarded.Add(1)
			io.WriteString(w, `{"allowed":true}`)
		}))
		defer destination.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
		defer server.Close()
		process, err := New(testConfig(t, server.URL))
		if err != nil {
			t.Fatal(err)
		}
		got := process.Authorize(context.Background(), ulid.Make().String(), contract.AuthorizeRequest{Operation: "publish", ContentID: "r1"})
		if got.Allowed || got.Reason != "unavailable" || forwarded.Load() != 0 {
			t.Fatalf("reply=%+v forwarded=%d", got, forwarded.Load())
		}
	})
}

func TestAuthorizeRejectsInvalidLocalInputWithoutHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, `{"allowed":true}`) }))
	defer server.Close()
	process, err := New(testConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../other", strings.ToLower(ulid.Make().String())} {
		got := process.Authorize(context.Background(), id, contract.AuthorizeRequest{Operation: "publish", ContentID: "r1"})
		if got.Allowed || got.Reason != "unavailable" {
			t.Fatalf("reply=%+v", got)
		}
	}
	got := process.Authorize(context.Background(), ulid.Make().String(), contract.AuthorizeRequest{Operation: "bad-operation", ContentID: "r1"})
	if got.Allowed || got.Reason != "unavailable" || calls.Load() != 0 {
		t.Fatalf("reply=%+v calls=%d", got, calls.Load())
	}
}
