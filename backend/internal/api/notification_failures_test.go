package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/notification"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const notificationFailurePath = "/api/notification-failures"

func requireNotificationFailures(t *testing.T, f *authFixture, query string) notificationFailurePage {
	t.Helper()
	status, body := projectRequest(t, f, "GET", notificationFailurePath+query, nil)
	if status != 200 {
		t.Fatalf("notification failures %s: status=%d body=%s", query, status, body)
	}
	var page notificationFailurePage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func appendNotificationFailure(t *testing.T, f *authFixture, executionID, kind, channel, reason string) int64 {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": kind, "channel": channel, "reason": reason,
		"error": "PRIVATE-PAYLOAD-ERROR", // Must never be forwarded, even from a legacy payload.
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.AppendExecutionEvent(context.Background(), f.db, executionID, model.EventNotificationFailed, payload)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestListNotificationFailuresAcrossTasksAndRetriesNewestFirst(t *testing.T) {
	f := newAuthFixture(t)
	seedStreamExecutions(t, f) // Two attempts belonging to task-001.
	const third = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	for _, statement := range []string{
		`UPDATE executions SET status = 'failed', error_message = 'PRIVATE-EXECUTION-ERROR' WHERE id = '` + streamExecution + `'`,
		`INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, collaboration_mode, status, idempotency_key, created_at, updated_at)
		 SELECT 'task-002', project_id, '別タスク', body, acceptance_criteria, collaboration_mode, 'completed', 'other-task', created_at, updated_at FROM tasks WHERE id = 'task-001'`,
		`INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		 VALUES ('` + third + `', 'task-002', 1, 'fresh', 'succeeded', '2026-10-26T00:00:00Z')`,
	} {
		if _, err := f.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	created, err := ParseTimestamp("2026-11-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	want := []notificationFailureResponse{}
	for i, execution := range []string{streamExecution, third, otherExecution, streamExecution, third} {
		kind := []string{"failed", "succeeded", "blocked", "timed_out", "interrupted"}[i]
		reason := []string{"permission_denied", "launch_failed", "timeout", "other", "other"}[i]
		channel := []string{"macos", "fake", "macos", "fake", "macos"}[i]
		id := appendNotificationFailure(t, f, execution, kind, channel, reason)
		taskID, title := "task-001", "Title"
		if execution == third {
			taskID, title = "task-002", "別タスク"
		}
		want = append([]notificationFailureResponse{{&id, &execution, nil, taskID, title, kind, channel, reason, created}}, want...)
		appendStreamEvent(t, f, execution, `{"text":"PRIVATE-LOG-ERROR"}`)
	}
	// Every other event type must be excluded, including sent notifications.
	for _, kind := range []model.ExecutionEventType{"status_changed", "context_pinned", "stage_started", "stage_finished", "check_result", "approval_requested", "approval_waived", "approval_consumed", "decision_opened", "decision_answered", "published", "notification_sent"} {
		if _, err := store.AppendExecutionEvent(context.Background(), f.db, third, kind, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	// Equal timestamps must still sort by global event ID.
	if _, err := f.db.Exec(`UPDATE execution_events SET created_at = '2026-11-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	status, body := projectRequest(t, f, "GET", notificationFailurePath, nil)
	var all notificationFailurePage
	if err := json.Unmarshal(body, &all); status != 200 || err != nil || !reflect.DeepEqual(all.Items, want) || all.NextCursor != nil {
		t.Fatalf("all failures: status=%d body=%s err=%v; want %+v", status, body, err, want)
	}
	if strings.Contains(string(body), "PRIVATE-") || !strings.Contains(string(body), `"nextCursor":null`) {
		t.Fatalf("unsafe or incorrect response: %s", body)
	}
	var shape struct{ Items []map[string]json.RawMessage }
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatal(err)
	}
	for _, item := range shape.Items {
		if len(item) != 9 || string(item["outboxId"]) != "null" {
			t.Fatalf("unexpected response fields: %v", item)
		}
	}
	for _, limit := range []int{1, 2, 3, 5} {
		var got []notificationFailureResponse
		query := fmt.Sprintf("?limit=%d", limit)
		for pageNumber := 0; ; pageNumber++ {
			if pageNumber >= len(want) {
				t.Fatal("pagination did not terminate")
			}
			page := requireNotificationFailures(t, f, query)
			if len(page.Items) > limit {
				t.Fatalf("limit=%d items=%d", limit, len(page.Items))
			}
			got = append(got, page.Items...)
			if page.NextCursor == nil {
				break
			}
			decoded, err := base64.RawURLEncoding.DecodeString(*page.NextCursor)
			last := *page.Items[len(page.Items)-1].EventID
			if err != nil || string(decoded) != fmt.Sprintf("notification-failures\n2026-11-01T00:00:00.000Z|e|%d", last) {
				t.Fatalf("invalid cursor %q: %v", decoded, err)
			}
			query = fmt.Sprintf("?limit=%d&cursor=%s", limit, url.QueryEscape(*page.NextCursor))
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("limit=%d missing/duplicate/unordered items: got %+v; want %+v", limit, got, want)
		}
	}
}

func TestListNotificationFailuresEmptyAndLimitDefaults(t *testing.T) {
	f := newAuthFixture(t)
	status, body := projectRequest(t, f, "GET", notificationFailurePath, nil)
	if status != 200 || string(body) != "{\"items\":[],\"nextCursor\":null}\n" {
		t.Fatalf("empty list: status=%d body=%s", status, body)
	}
	seedStreamExecutions(t, f)
	for range 201 {
		appendNotificationFailure(t, f, streamExecution, "failed", "macos", "other")
	}
	for query, count := range map[string]int{"": 50, "?limit=200": 200} {
		page := requireNotificationFailures(t, f, query)
		if len(page.Items) != count || page.NextCursor == nil {
			t.Fatalf("%s: count=%d cursor=%v", query, len(page.Items), page.NextCursor)
		}
		tail := requireNotificationFailures(t, f, "?limit=200&cursor="+url.QueryEscape(*page.NextCursor))
		if len(tail.Items) != 201-count || tail.NextCursor != nil {
			t.Fatalf("tail: count=%d cursor=%v", len(tail.Items), tail.NextCursor)
		}
	}
}

func TestListNotificationFailuresRejectsInvalidPagination(t *testing.T) {
	f := newAuthFixture(t)
	for _, query := range []string{"limit=0", "limit=-1", "limit=201", "limit=999999999999999999999", "limit=", "limit=no", "limit=1&limit=2",
		"cursor=invalid!", "cursor=a&cursor=b", "limit=2&cursor=%zz"} {
		status, body := projectRequest(t, f, "GET", notificationFailurePath+"?"+query, nil)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	for _, raw := range []string{"tasks\ntask-001", "notification-failures\n0", "notification-failures\n-1",
		"notification-failures\n+1", "notification-failures\n1.5", "notification-failures\nabc", "notification-failures\n9223372036854775808",
		"notification-failures\n2026-11-01T00:00:00.000Z|e|0", "notification-failures\n2026-11-01T00:00:00.000Z|e|-1",
		"notification-failures\n2026-11-01T00:00:00.000Z|e|+1", "notification-failures\n2026-11-01T00:00:00.000Z|e|9223372036854775808",
		"notification-failures\n2026-11-01T00:00:00.000Z|x|1", "notification-failures\n2026-11-01T00:00:00.000Z|o|",
		"notification-failures\n2026-11-01T00:00:00Z|e|1", "notification-failures\n2026-13-01T00:00:00.000Z|e|1",
		"notification-failures\n2026-11-01T00:00:00.000Z|e"} {
		token := base64.RawURLEncoding.EncodeToString([]byte(raw))
		status, body := projectRequest(t, f, "GET", notificationFailurePath+"?cursor="+token, nil)
		requireProjectError(t, status, body, 400, ValidationFailed, "cursor", "invalid")
	}
}

func TestListNotificationFailuresCursorExcludesNewerInsertions(t *testing.T) {
	f := newAuthFixture(t)
	seedStreamExecutions(t, f)
	oldest := appendNotificationFailure(t, f, streamExecution, "failed", "macos", "other")
	for range 2 {
		appendNotificationFailure(t, f, otherExecution, "blocked", "macos", "timeout")
	}
	first := requireNotificationFailures(t, f, "?limit=2")
	if len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("first page=%+v", first)
	}
	appendNotificationFailure(t, f, streamExecution, "failed", "macos", "launch_failed")
	tail := requireNotificationFailures(t, f, "?limit=2&cursor="+url.QueryEscape(*first.NextCursor))
	if len(tail.Items) != 1 || *tail.Items[0].EventID != oldest || tail.NextCursor != nil {
		t.Fatalf("new insertion changed continuation: %+v", tail)
	}
}

func TestListNotificationFailuresStoredDataErrorsStayInternal(t *testing.T) {
	for _, statement := range []string{
		`UPDATE execution_events SET created_at = 'PRIVATE-INVALID-TIMESTAMP'`,
		`UPDATE execution_events SET payload = 'PRIVATE-INVALID-JSON'`,
	} {
		t.Run(statement, func(t *testing.T) {
			f := newAuthFixture(t)
			seedStreamExecutions(t, f)
			appendNotificationFailure(t, f, streamExecution, "failed", "macos", "other")
			if _, err := f.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			status, body := projectRequest(t, f, "GET", notificationFailurePath, nil)
			assertAuthError(t, status, body, 500, Internal)
			if strings.Contains(string(body), "PRIVATE-") {
				t.Fatalf("stored error exposed: %s", body)
			}
		})
	}
}

func TestListNotificationFailuresRequiresOwnerAuthentication(t *testing.T) {
	f := newAuthFixture(t)
	status, body := f.request(t, "GET", notificationFailurePath, false, "", nil)
	assertAuthError(t, status, body, 401, Unauthenticated)
	token, err := runnerauth.Register(context.Background(), f.db, "failure-reader", "Runner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, withCookie := range []bool{false, true} {
		status, body = f.request(t, "GET", notificationFailurePath, withCookie, "", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
		})
		assertAuthError(t, status, body, 403, Forbidden)
	}
}

func TestListNotificationFailuresFromDispatcher(t *testing.T) {
	f, token, executionID, _ := resultFixture(t, "working")
	status, body := postResult(t, f, token, executionID, resultBody("working", "failed"))
	if status != 204 {
		t.Fatalf("result: status=%d body=%s", status, body)
	}
	d := notificationDispatcher(f, &recordingNotifier{err: fmt.Errorf("PRIVATE-TRANSPORT-ERROR: %w", notification.ErrPermissionDenied)})
	if err := d.DispatchPending(context.Background()); err == nil {
		t.Fatal("expected transport failure")
	}
	page := requireNotificationFailures(t, f, "")
	if len(page.Items) != 1 || *page.Items[0].ExecutionID != executionID || page.Items[0].Kind != "failed" ||
		page.Items[0].Channel != "fake" || page.Items[0].Reason != "permission_denied" {
		t.Fatalf("dispatcher failure missing: %+v", page)
	}
	_, body = projectRequest(t, f, "GET", notificationFailurePath, nil)
	if strings.Contains(string(body), "PRIVATE-") || strings.Contains(string(body), "Harness exited") {
		t.Fatalf("error detail exposed: %s", body)
	}
}
