package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const (
	streamExecution = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	otherExecution  = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	streamPath      = "/api/executions/" + streamExecution + "/events"
)

func seedStreamExecutions(t *testing.T, f *authFixture) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO runners (id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
		 VALUES ('seed-runner', 'Runner', 'unused', '/worktrees', 'offline', '', '2026-10-26T00:00:00Z')`,
		`INSERT INTO projects (id, name, local_repo_path, git_remote_url, runner_id, base_branch,
		 default_role, context_location, copy_allowlist, allowed_operations,
		 approval_policy, timeouts, created_at, updated_at)
		 VALUES ('project-a', 'Project', '/repo', '', 'seed-runner', 'main', 'developer',
		 'AGENTS.md', '[]', '{}', '{}', '{}', '2026-10-26T00:00:00Z', '2026-10-26T00:00:00Z')`,
		`INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, short_name, task_type,
		 collaboration_mode, status, idempotency_key, created_at, updated_at)
		 VALUES ('task-001', 'project-a', 'Title', '', 'events resume', 'sse', 'development', 'solo',
		 'queued', 'stream-test', '2026-10-26T00:00:00Z', '2026-10-26T00:00:00Z')`,
	} {
		if _, err := f.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range []string{streamExecution, otherExecution} {
		if _, err := f.db.Exec(`INSERT INTO executions (id, task_id, seq, mode, status, created_at)
			VALUES (?, 'task-001', ?, 'fresh', 'pending', '2026-10-26T00:00:00Z')`, id, i+1); err != nil {
			t.Fatal(err)
		}
	}
}

func appendStreamEvent(t *testing.T, f *authFixture, execution, payload string) int64 {
	t.Helper()
	id, err := store.AppendExecutionEvent(context.Background(), f.db, execution, model.EventLog, json.RawMessage(payload))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The deadline only bounds failures. Success is synchronized by receiving real
// HTTP headers/frames, never by sleeping for the poll interval.
func openEventStream(t *testing.T, f *authFixture, lastID string) (*http.Response, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	r, err := http.NewRequestWithContext(ctx, "GET", f.server.URL+streamPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.AddCookie(f.cookie)
	if lastID != "" {
		r.Header.Set("Last-Event-ID", lastID)
	}
	response, err := f.server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Cache-Control") != "no-store" {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("SSE status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
	}
	return response, bufio.NewReader(response.Body)
}

func readStreamEvent(t *testing.T, reader *bufio.Reader, id int64, payload string) {
	t.Helper()
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read event %d: %v (partial frame %q)", id, err, frame.String())
		}
		frame.WriteString(line)
		if line == "\n" {
			break
		}
	}
	compact, err := json.Marshal(json.RawMessage(payload))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("id: %d\nevent: log\ndata: %s\n\n", id, compact)
	if frame.String() != want {
		t.Fatalf("SSE frame=%q; want %q (no duplicates, gaps, or other Execution's events)", frame.String(), want)
	}
}

func TestSSEDisconnectAndLastEventIDResume(t *testing.T) {
	f := newAuthFixture(t)
	seedStreamExecutions(t, f)
	first, reader := openEventStream(t, f, "") // Empty streams must flush headers.
	payload := "{\n  \"text\": \"first\\nsecond line 日本語\"\n}"
	firstID := appendStreamEvent(t, f, streamExecution, payload)
	readStreamEvent(t, reader, firstID, payload)
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	// Publish while disconnected, interleaving a different Execution. Global
	// sequence gaps must not mean gaps or cross-talk in this Execution's stream.
	secondID := appendStreamEvent(t, f, streamExecution, `{"text":"disconnected-1"}`)
	appendStreamEvent(t, f, otherExecution, `{"text":"private to another execution"}`)
	thirdID := appendStreamEvent(t, f, streamExecution, `{"text":"disconnected-2"}`)
	_, resumed := openEventStream(t, f, strconv.FormatInt(firstID, 10))
	readStreamEvent(t, resumed, secondID, `{"text":"disconnected-1"}`)
	// Publish while replay is in progress, crossing from replay to live delivery.
	fourthID := appendStreamEvent(t, f, streamExecution, `{"text":"live"}`)
	readStreamEvent(t, resumed, thirdID, `{"text":"disconnected-2"}`)
	readStreamEvent(t, resumed, fourthID, `{"text":"live"}`)

	// A second subscriber at the latest ID waits for new events, and receiving
	// from one connection cannot consume events destined for the other.
	_, latest := openEventStream(t, f, strconv.FormatInt(fourthID, 10))
	fifthID := appendStreamEvent(t, f, streamExecution, `{"text":"both subscribers"}`)
	readStreamEvent(t, resumed, fifthID, `{"text":"both subscribers"}`)
	readStreamEvent(t, latest, fifthID, `{"text":"both subscribers"}`)
}

func TestSSERetainedHistoryAcrossBatches(t *testing.T) {
	f := newAuthFixture(t)
	seedStreamExecutions(t, f)
	var ids []int64
	for i := 0; i < 2*eventBatchSize+1; i++ {
		ids = append(ids, appendStreamEvent(t, f, streamExecution, fmt.Sprintf(`{"seq":%d}`, i)))
	}
	_, reader := openEventStream(t, f, "")
	for i, id := range ids {
		readStreamEvent(t, reader, id, fmt.Sprintf(`{"seq":%d}`, i))
	}
	liveID := appendStreamEvent(t, f, streamExecution, `{"text":"after history"}`)
	readStreamEvent(t, reader, liveID, `{"text":"after history"}`)
}

func TestSSEValidationAndUnavailableCursors(t *testing.T) {
	f := newAuthFixture(t)
	seedStreamExecutions(t, f)
	removed := appendStreamEvent(t, f, streamExecution, `{}`)
	other := appendStreamEvent(t, f, otherExecution, `{}`)
	retained := appendStreamEvent(t, f, streamExecution, `{}`)
	if _, err := f.db.Exec(`DELETE FROM execution_events WHERE id = ?`, removed); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		headers    []string
		status     int
		code       Code
	}{
		{"empty", streamPath, []string{""}, 400, ValidationFailed},
		{"zero", streamPath, []string{"0"}, 400, ValidationFailed},
		{"negative", streamPath, []string{"-1"}, 400, ValidationFailed},
		{"leading zero", streamPath, []string{"01"}, 400, ValidationFailed},
		{"sign", streamPath, []string{"+1"}, 400, ValidationFailed},
		{"fraction", streamPath, []string{"1.0"}, 400, ValidationFailed},
		{"ULID", streamPath, []string{streamExecution}, 400, ValidationFailed},
		{"overflow", streamPath, []string{"9223372036854775808"}, 400, ValidationFailed},
		{"duplicate", streamPath, []string{"1", "1"}, 400, ValidationFailed},
		{"comma joined", streamPath, []string{"1, 2"}, 400, ValidationFailed},
		{"removed", streamPath, []string{strconv.FormatInt(removed, 10)}, 422, PreconditionFailed},
		{"other Execution", streamPath, []string{strconv.FormatInt(other, 10)}, 422, PreconditionFailed},
		{"future", streamPath, []string{"9223372036854775807"}, 422, PreconditionFailed},
		{"invalid Execution ID", "/api/executions/not-a-ulid/events", nil, 400, ValidationFailed},
		{"missing Execution", "/api/executions/01ARZ3NDEKTSV4RRFFQ69G5FAX/events", nil, 404, NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := f.request(t, "GET", tc.path, true, "", func(r *http.Request) {
				for _, header := range tc.headers {
					r.Header.Add("Last-Event-ID", header)
				}
			})
			assertAuthError(t, status, body, tc.status, tc.code)
			if tc.path == streamPath && !strings.Contains(string(body), `"field":"Last-Event-ID"`) {
				t.Fatalf("missing cursor error detail: %s", body)
			}
		})
	}
	// With no explicit resume point, retained history can still be read.
	_, reader := openEventStream(t, f, "")
	readStreamEvent(t, reader, retained, `{}`)
}

func TestSSEHeadDoesNotRetainAStream(t *testing.T) {
	f := newAuthFixture(t)
	seedStreamExecutions(t, f)
	// Observe handler return, since a HEAD client itself sees headers even if
	// the handler accidentally remains in its streaming loop forever.
	returned := make(chan struct{})
	base := NewHandler(f.db, nil, f.auth)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base.ServeHTTP(w, r)
		close(returned)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, "HEAD", server.URL+streamPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the production authentication authority despite this test listener.
	r.Host = f.auth.host
	r.AddCookie(f.cookie)
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("HEAD status=%d headers=%v", response.StatusCode, response.Header)
	}
	select {
	case <-returned:
	case <-ctx.Done():
		t.Fatal("HEAD retained an SSE request")
	}
}

func TestSSEOwnerAuthenticationBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		cookie       bool
		status       int
		code         Code
	}{
		{"no cookie", "invalid_session", false, 401, Unauthenticated},
		{"Runner token", "wrong_auth_class", false, 403, Forbidden},
		{"foreign Origin", "origin_mismatch", true, 403, Forbidden},
		{"foreign Host", "host_mismatch", true, 403, Forbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			seedStreamExecutions(t, f)
			token := registerAuthRunner(t, f)
			status, body := f.request(t, "GET", streamPath, tc.cookie, "", func(r *http.Request) {
				switch tc.name {
				case "Runner token":
					r.Header.Set("Authorization", "Bearer "+token)
				case "foreign Origin":
					r.Header.Set("Origin", "http://evil.example")
				case "foreign Host":
					r.Host = "evil.example"
				}
			})
			assertAuthError(t, status, body, tc.status, tc.code)
			f.assertAudit(t, "GET", streamPath, tc.reason, tc.status, tc.code)
		})
	}
}

func TestSSEDatabaseFailureBeforeAndAfterHeaders(t *testing.T) {
	t.Run("before headers", func(t *testing.T) {
		f := newAuthFixture(t)
		seedStreamExecutions(t, f)
		if err := f.db.Close(); err != nil {
			t.Fatal(err)
		}
		status, body := f.request(t, "GET", streamPath, true, "", nil)
		assertAuthError(t, status, body, 500, Internal)
	})
	t.Run("corrupt stored payload", func(t *testing.T) {
		f := newAuthFixture(t)
		seedStreamExecutions(t, f)
		if _, err := f.db.Exec(`INSERT INTO execution_events (execution_id, type, payload, created_at)
			VALUES (?, 'log', 'invalid-secret-canary', '2026-10-26T00:00:00Z')`, streamExecution); err != nil {
			t.Fatal(err)
		}
		status, body := f.request(t, "GET", streamPath, true, "", nil)
		assertAuthError(t, status, body, 500, Internal)
		if strings.Contains(string(body), "canary") {
			t.Fatal("stored payload leaked through error")
		}
	})
	t.Run("after headers", func(t *testing.T) {
		f := newAuthFixture(t)
		seedStreamExecutions(t, f)
		response, _ := openEventStream(t, f, "")
		if err := f.db.Close(); err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil || len(body) != 0 {
			t.Fatalf("stream must close without JSON errors: body=%q err=%v", body, err)
		}
	})
}
