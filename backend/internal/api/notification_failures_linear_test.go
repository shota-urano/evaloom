package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearfeedback"
	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// failingLinear answers every comment with a temporary failure whose text
// must never reach the API response.
type failingLinear struct{ creates int }

func (c *failingLinear) CheckKey() error { return nil }
func (c *failingLinear) CreateComment(context.Context, string, string) (string, error) {
	c.creates++
	return "", fmt.Errorf("PRIVATE-LINEAR-ERROR: %w", &linear.Error{Op: "comment", Class: linear.ClassNetwork})
}
func (c *failingLinear) Issue(context.Context, string) (linear.Issue, error) {
	return linear.Issue{ID: "issue"}, nil
}
func (c *failingLinear) IssueComments(context.Context, string) ([]linear.Comment, error) {
	return nil, errors.New("PRIVATE-LINEAR-ERROR")
}

// insertLinearTask adds a Linear task with its Issue link. heldCause, when
// not empty, makes it a task held at intake (blocked, no Execution).
func insertLinearTask(t *testing.T, f *authFixture, id, title, heldCause string) {
	t.Helper()
	status, cause := "queued", any(nil)
	if heldCause != "" {
		status, cause = "blocked", heldCause
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, short_name, task_type,
			collaboration_mode, status, idempotency_key, created_at, updated_at, source, intake_block_cause)
			VALUES (?, 'project-a', ?, '', 'criteria', 'linear', 'development', 'solo', ?, ?,
			'2026-10-26T00:00:00Z', '2026-10-26T00:00:00Z', 'linear', ?)`, []any{id, title, status, "key-" + id, cause}},
		{`INSERT INTO linear_issue_links (id, linear_issue_id, issue_identifier, issue_url, team_id, task_id,
			attach_seq, created_at) VALUES (?, ?, 'ENG-1', 'https://linear.app/x/ENG-1', 'team-a', ?, 1,
			'2026-10-26T00:00:00Z')`, []any{"link-" + id, "issue-" + id, id}},
	} {
		if _, err := f.db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func enqueueTrigger(t *testing.T, f *authFixture, trigger linearoutbox.Trigger) {
	t.Helper()
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := linearoutbox.EnqueueTx(context.Background(), tx, trigger); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// 受け入れ基準 (spec 16 3.4.2, spec 12 5章, Q82): intake_blocked・
// missing_acceptance_criteria・保留したタスクの中止の送信が上限で failed に
// なると、GET /api/notification-failures に taskId・taskTitle・kind・
// channel=linear・reason が入った項目が出て、eventId と executionId が null、
// outboxId が入る。Execution のある契機の失敗は execution_events の項目と
// して1回だけ出て、outbox の行から二重に出ない。応答にエラーの文字列と
// コメントの本文が入らない。
func TestNotificationFailuresListLinearFailuresWithoutExecution(t *testing.T) {
	f := newAuthFixture(t)
	seedStreamExecutions(t, f)
	insertLinearTask(t, f, "task-101", "保留した Issue", "routing_no_label")
	insertLinearTask(t, f, "task-102", "受け入れ条件の無い Issue", "missing_acceptance_criteria")
	insertLinearTask(t, f, "task-103", "中止した Issue", "routing_unknown_label")
	insertLinearTask(t, f, "task-104", "実行した Issue", "")
	if _, err := f.db.Exec(`INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		VALUES ('01ARZ3NDEKTSV4RRFFQ69G5FB4', 'task-104', 1, 'fresh', 'blocked', '2026-10-26T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	enqueueTrigger(t, f, linearoutbox.Trigger{TaskID: "task-101", Kind: linearoutbox.KindIntakeBlocked, TargetID: "routing_no_label"})
	enqueueTrigger(t, f, linearoutbox.Trigger{TaskID: "task-102", Kind: linearoutbox.KindMissingAcceptanceCriteria,
		TargetID: "missing_acceptance_criteria"})
	// The cancellation of a held task takes the production path.
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CancelTaskTx(context.Background(), tx, store.TaskCancellation{TaskID: "task-103", Actor: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	enqueueTrigger(t, f, linearoutbox.Trigger{TaskID: "task-104", Kind: linearoutbox.KindBlocked,
		TargetID: "01ARZ3NDEKTSV4RRFFQ69G5FB4", ExecutionID: "01ARZ3NDEKTSV4RRFFQ69G5FB4"})

	now := time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)
	client := &failingLinear{}
	worker := &linearfeedback.Worker{DB: f.db, Client: client, Now: func() time.Time { return now }}
	for range linearfeedback.MaxAttempts {
		if err := worker.SendDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
	}
	if client.creates != 4*linearfeedback.MaxAttempts {
		t.Fatalf("comment sends = %d; want %d", client.creates, 4*linearfeedback.MaxAttempts)
	}
	if n := countLinearRows(t, f, `SELECT count(*) FROM linear_outbox WHERE status = 'failed'
		AND failure_reason = 'retries_exhausted' AND failed_at IS NOT NULL`); n != 4 {
		t.Fatalf("failed rows = %d; want 4", n)
	}

	status, body := projectRequest(t, f, "GET", notificationFailurePath, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	for _, private := range []string{"PRIVATE-", "Evaloom:", ":intake_blocked:", "管理画面のタスク ID", "実行ステータス"} {
		if strings.Contains(string(body), private) {
			t.Fatalf("response contains %q: %s", private, body)
		}
	}
	var raw struct{ Items []map[string]json.RawMessage }
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	var page notificationFailurePage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 4 || len(raw.Items) != 4 {
		t.Fatalf("items = %s; want 3 outbox rows and 1 event", body)
	}
	outboxIDs := map[string]string{}
	rows, err := f.db.Query(`SELECT task_id, id FROM linear_outbox WHERE action = 'comment'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var taskID, id string
		if err := rows.Scan(&taskID, &id); err != nil {
			t.Fatal(err)
		}
		outboxIDs[taskID] = id
	}
	rows.Close()
	want := map[string]struct{ title, kind string }{
		"task-101": {"保留した Issue", "intake_blocked"},
		"task-102": {"受け入れ条件の無い Issue", "missing_acceptance_criteria"},
		"task-103": {"中止した Issue", "cancelled"},
	}
	seen := map[string]int{}
	for i, item := range page.Items {
		seen[item.TaskID]++
		if len(raw.Items[i]) != 9 || item.Channel != "linear" || item.Reason != "retries_exhausted" {
			t.Fatalf("item %d = %s", i, raw.Items[i])
		}
		if item.TaskID == "task-104" {
			if item.EventID == nil || item.ExecutionID == nil || *item.ExecutionID != "01ARZ3NDEKTSV4RRFFQ69G5FB4" ||
				item.OutboxID != nil || string(raw.Items[i]["outboxId"]) != "null" || item.Kind != "blocked" {
				t.Fatalf("Execution trigger item = %s", raw.Items[i])
			}
			continue
		}
		w, ok := want[item.TaskID]
		if !ok || item.TaskTitle != w.title || item.Kind != w.kind || item.OutboxID == nil || *item.OutboxID != outboxIDs[item.TaskID] ||
			string(raw.Items[i]["eventId"]) != "null" || string(raw.Items[i]["executionId"]) != "null" {
			t.Fatalf("outbox item = %s", raw.Items[i])
		}
	}
	if !reflect.DeepEqual(seen, map[string]int{"task-101": 1, "task-102": 1, "task-103": 1, "task-104": 1}) {
		t.Fatalf("items per task = %v; want each once", seen)
	}
	var events int
	if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE type = ?`, model.EventNotificationFailed).Scan(&events); err != nil || events != 1 {
		t.Fatalf("notification_failed events = %d, %v; want 1", events, err)
	}
}

func countLinearRows(t *testing.T, f *authFixture, query string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 受け入れ基準 (spec 12 5章, Q82): macOS 通知の失敗と Linear の失敗が
// 混ざった一覧が記録した時刻（execution_events.created_at、
// linear_outbox.failed_at）の新しい順に並び、cursor でページを続けて読んで
// 欠けも重複も無い。同じ時刻の中は出所と ID で順を決める。
func TestNotificationFailuresMixedSourcesOrderAndCursor(t *testing.T) {
	f := newAuthFixture(t)
	seedStreamExecutions(t, f)
	insertLinearTask(t, f, "task-101", "Linear", "routing_no_label")
	event := func(at string) int64 {
		id := appendNotificationFailure(t, f, streamExecution, "failed", "macos", "permission_denied")
		if _, err := f.db.Exec(`UPDATE execution_events SET created_at = ? WHERE id = ?`, at, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	outbox := func(id, status, at string, execution any) {
		if _, err := f.db.Exec(`INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, dedupe_key, body,
			status, attempts, created_at, execution_id, failure_reason, failed_at)
			VALUES (?, 'task-101', 'issue-task-101', 'intake_blocked', ?, 'PRIVATE-COMMENT-BODY', ?, 5,
			'2026-10-01T00:00:00Z', ?, ?, ?)`, id, "task-101:intake_blocked:"+id, status, execution,
			map[bool]any{true: "auth", false: nil}[status == "failed"], map[bool]any{true: at, false: nil}[status == "failed"]); err != nil {
			t.Fatal(err)
		}
	}
	// Events are stored with second precision, outbox rows with
	// nanoseconds; both must compare by time, not by text.
	e1 := event("2026-11-01T00:00:00Z")
	e2 := event("2026-11-01T00:00:02Z")
	e3 := event("2026-11-01T00:00:01Z") // A newer ID with an older time.
	e4 := event("2026-11-01T00:00:01Z")
	outbox("01ARZ3NDEKTSV4RRFFQ69G5FC1", "failed", "2026-11-01T00:00:01.5Z", nil)
	outbox("01ARZ3NDEKTSV4RRFFQ69G5FC2", "failed", "2026-11-01T00:00:00.999999999Z", nil)
	outbox("01ARZ3NDEKTSV4RRFFQ69G5FC3", "failed", "2026-11-01T00:00:01Z", nil) // Same time as e3 and e4.
	outbox("01ARZ3NDEKTSV4RRFFQ69G5FC4", "failed", "2026-11-01T00:00:01.000000000Z", nil)
	outbox("01ARZ3NDEKTSV4RRFFQ69G5FC5", "failed", "2026-11-01T00:00:03Z", streamExecution) // Listed as its event only.
	outbox("01ARZ3NDEKTSV4RRFFQ69G5FC6", "pending", "", nil)                                // Not a failure.
	outbox("01ARZ3NDEKTSV4RRFFQ69G5FC7", "sent", "", nil)                                   // Not a failure.

	type key struct {
		event  int64
		outbox string
	}
	want := []key{
		{event: e2},                            // 00:00:02
		{outbox: "01ARZ3NDEKTSV4RRFFQ69G5FC1"}, // 00:00:01.5
		{outbox: "01ARZ3NDEKTSV4RRFFQ69G5FC4"}, // 00:00:01, outbox before events, ID descending
		{outbox: "01ARZ3NDEKTSV4RRFFQ69G5FC3"},
		{event: e4},
		{event: e3},
		{outbox: "01ARZ3NDEKTSV4RRFFQ69G5FC2"}, // 00:00:00.999
		{event: e1},                            // 00:00:00
	}
	keyOf := func(item notificationFailureResponse) key {
		if item.EventID != nil {
			return key{event: *item.EventID}
		}
		return key{outbox: *item.OutboxID}
	}
	all := requireNotificationFailures(t, f, "")
	var got []key
	for _, item := range all.Items {
		got = append(got, keyOf(item))
	}
	if !reflect.DeepEqual(got, want) || all.NextCursor != nil {
		t.Fatalf("order = %+v; want %+v", got, want)
	}
	for i := 1; i < len(all.Items); i++ {
		if all.Items[i].CreatedAt.After(all.Items[i-1].CreatedAt.Time) {
			t.Fatalf("item %d is newer than item %d", i, i-1)
		}
	}
	if item := all.Items[1]; item.CreatedAt.Format(time.RFC3339Nano) != "2026-11-01T00:00:01.5Z" || item.Reason != "auth" {
		t.Fatalf("outbox item = %+v; want failed_at and failure_reason", item)
	}
	for limit := 1; limit <= len(want); limit++ {
		var paged []key
		query := fmt.Sprintf("?limit=%d", limit)
		for pageNumber := 0; ; pageNumber++ {
			if pageNumber > len(want) {
				t.Fatal("pagination did not terminate")
			}
			page := requireNotificationFailures(t, f, query)
			for _, item := range page.Items {
				paged = append(paged, keyOf(item))
			}
			if page.NextCursor == nil {
				break
			}
			query = fmt.Sprintf("?limit=%d&cursor=%s", limit, url.QueryEscape(*page.NextCursor))
		}
		if !reflect.DeepEqual(paged, want) {
			t.Fatalf("limit=%d: pages = %+v; want %+v", limit, paged, want)
		}
	}
	_, body := projectRequest(t, f, "GET", notificationFailurePath, nil)
	if strings.Contains(string(body), "PRIVATE-") {
		t.Fatalf("response exposes a comment body or error: %s", body)
	}
}
