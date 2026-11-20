package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"testing"
)

type taskListPage struct {
	Items      []taskSummaryView `json:"items"`
	NextCursor string            `json:"nextCursor"`
}

// Uses the production router directly (newAuthFixture reserves /api/tasks).
func listTaskRequest(t *testing.T, f *authFixture, query string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest("GET", f.server.URL+"/api/tasks"+query, nil)
	r.AddCookie(f.cookie)
	r.Header.Set("Origin", f.server.URL)
	w := httptest.NewRecorder()
	NewHandler(f.db, nil, f.auth).ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func requireTaskList(t *testing.T, f *authFixture, query string) taskListPage {
	t.Helper()
	status, body := listTaskRequest(t, f, query)
	if status != 200 {
		t.Fatalf("GET /api/tasks%s status=%d body=%s", query, status, body)
	}
	var page taskListPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func taskIDs(page taskListPage) []string {
	ids := []string{}
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestListTasksFiltersByStatusAndProjectNewestFirst(t *testing.T) {
	f := taskFixture(t)
	status, body := postAssignment(t, f, "project-b", "developer", "harness-a", "1.2.0")
	requireAssignment(t, status, body, 200)
	for i, project := range []string{"project-a", "project-b", "project-a"} {
		input := manualTaskInput()
		input["projectId"] = project
		status, body := taskRequest(t, f, fmt.Sprintf("list-%d", i), input, nil)
		requireTask(t, status, body, 201)
	}
	if status, body := requestCancellation(t, f, "task-003"); status != 204 {
		t.Fatalf("cancel status=%d body=%s", status, body)
	}

	all := requireTaskList(t, f, "")
	if got := fmt.Sprint(taskIDs(all)); got != "[task-003 task-002 task-001]" || all.NextCursor != "" {
		t.Fatalf("all=%s cursor=%q", got, all.NextCursor)
	}
	first := all.Items[2]
	if first.LatestExecution == nil || first.LatestExecution.Seq != 1 || first.LatestExecution.Status != "pending" {
		t.Fatalf("task-001 summary=%+v", first)
	}
	if e := all.Items[0].LatestExecution; all.Items[0].Status != "cancelled" || e == nil || e.Status != "cancelled" {
		t.Fatalf("task-003 summary=%+v", all.Items[0])
	}

	for query, want := range map[string]string{
		"?projectId=project-a":                  "[task-003 task-001]",
		"?status=queued":                        "[task-002 task-001]",
		"?status=queued&projectId=project-a":    "[task-001]",
		"?status=cancelled&projectId=project-b": "[]",
		"?status=&projectId=":                   "[task-003 task-002 task-001]",
	} {
		if got := fmt.Sprint(taskIDs(requireTaskList(t, f, query))); got != want {
			t.Fatalf("%s => %s; want %s", query, got, want)
		}
	}

	page1 := requireTaskList(t, f, "?limit=2")
	page2 := requireTaskList(t, f, "?limit=2&cursor="+page1.NextCursor)
	if fmt.Sprint(taskIDs(page1)) != "[task-003 task-002]" || page1.NextCursor == "" ||
		fmt.Sprint(taskIDs(page2)) != "[task-001]" || page2.NextCursor != "" {
		t.Fatalf("pages=%v %v", taskIDs(page1), taskIDs(page2))
	}
}

func TestListTasksRejectsInvalidFilters(t *testing.T) {
	f := taskFixture(t)
	for _, query := range []string{"?status=unknown", "?projectId=Bad_ID", "?harnessId=Bad_ID", "?harnessId=harness-a&harnessId=harness-b", "?status=queued&status=failed", "?limit=0", "?cursor=bad"} {
		if status, body := listTaskRequest(t, f, query); status != 400 {
			t.Fatalf("%s status=%d body=%s", query, status, body)
		}
	}
}

func TestListTasksFiltersByDirectHarnessDestination(t *testing.T) {
	f := taskFixture(t)
	// Use the same harness as project-a, to distinguish direct routing from
	// the project's assignment and the Execution's claim-time harness pin.
	if _, err := f.db.Exec(`UPDATE harnesses SET default_profile_id =
		(SELECT profile_id FROM projects WHERE id = 'project-a') WHERE id = 'harness-a'`); err != nil {
		t.Fatal(err)
	}
	for i, direct := range []bool{false, true, true, true} {
		input := manualTaskInput()
		if direct {
			delete(input, "projectId")
			input["harnessId"] = "harness-a"
		}
		status, body := taskRequest(t, f, fmt.Sprintf("harness-list-%d", i), input, nil)
		requireTask(t, status, body, 201)
	}
	if status, body := requestCancellation(t, f, "task-003"); status != 204 {
		t.Fatalf("cancel status=%d body=%s", status, body)
	}
	for query, want := range map[string]string{
		"?harnessId=harness-a":                     "[task-004 task-003 task-002]",
		"?harnessId=harness-a&status=queued":       "[task-004 task-002]",
		"?harnessId=harness-a&status=cancelled":    "[task-003]",
		"?harnessId=harness-a&projectId=project-a": "[]",
		"?harnessId=missing":                       "[]",
		"?harnessId=":                              "[task-004 task-003 task-002 task-001]",
	} {
		if got := fmt.Sprint(taskIDs(requireTaskList(t, f, query))); got != want {
			t.Fatalf("%s => %s; want %s", query, got, want)
		}
	}
	page1 := requireTaskList(t, f, "?harnessId=harness-a&status=queued&limit=1")
	page2 := requireTaskList(t, f, "?harnessId=harness-a&status=queued&limit=1&cursor="+page1.NextCursor)
	if fmt.Sprint(taskIDs(page1)) != "[task-004]" || page1.NextCursor == "" ||
		fmt.Sprint(taskIDs(page2)) != "[task-002]" || page2.NextCursor != "" {
		t.Fatalf("pages=%v %v", taskIDs(page1), taskIDs(page2))
	}
}

func TestListTasksReturnsLatestExecutionOutputs(t *testing.T) {
	f := taskFixture(t)
	for i := 0; i < 3; i++ {
		status, body := taskRequest(t, f, fmt.Sprintf("outputs-%d", i), manualTaskInput(), nil)
		requireTask(t, status, body, 201)
	}
	old := `[{"kind":"old","label":"Earlier output","url":"https://example.invalid/old"}]`
	latest := `[{"kind":"report","label":"調査結果","url":"https://example.invalid/report?a=1&b=2"},{"kind":"post","label":"投稿","url":"https://example.invalid/post"}]`
	if _, err := f.db.Exec(`UPDATE executions SET outputs = ? WHERE task_id IN ('task-001', 'task-002')`, old); err != nil {
		t.Fatal(err)
	}
	// seq, rather than ID or creation time, defines the latest Execution.
	if _, err := f.db.Exec(`INSERT INTO executions (id, task_id, seq, mode, status, outputs, created_at)
		VALUES ('01AAAAAAAAAAAAAAAAAAAAAAA1', 'task-001', 2, 'fresh', 'succeeded', ?, '2026-01-01T00:00:00Z'),
		('01AAAAAAAAAAAAAAAAAAAAAAA2', 'task-002', 2, 'fresh', 'pending', NULL, '2026-01-01T00:00:00Z')`, latest); err != nil {
		t.Fatal(err)
	}
	status, body := listTaskRequest(t, f, "")
	if status != 200 {
		t.Fatalf("list status=%d body=%s", status, body)
	}
	// Decode independently of the view so a missing outputs key cannot pass.
	var page struct {
		Items []struct {
			ID              string `json:"id"`
			LatestExecution struct {
				Seq     int             `json:"seq"`
				Outputs json.RawMessage `json:"outputs"`
			} `json:"latestExecution"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("items=%s", body)
	}
	for i, want := range []struct {
		id, outputs string
		seq         int
	}{{"task-003", "[]", 1}, {"task-002", "[]", 2}, {"task-001", latest, 2}} {
		got := page.Items[i]
		// Compare JSON values, allowing the encoder to escape URL characters.
		var gotValue, wantValue any
		if err := json.Unmarshal(got.LatestExecution.Outputs, &gotValue); err != nil {
			t.Fatalf("%s outputs=%s: %v", got.ID, got.LatestExecution.Outputs, err)
		}
		if err := json.Unmarshal([]byte(want.outputs), &wantValue); err != nil {
			t.Fatal(err)
		}
		if got.ID != want.id || got.LatestExecution.Seq != want.seq || !reflect.DeepEqual(gotValue, wantValue) {
			t.Fatalf("item=%+v outputs=%s; want %+v", got, got.LatestExecution.Outputs, want)
		}
	}
}
