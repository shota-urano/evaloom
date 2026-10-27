package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
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
	requireAssignment(t, status, body, 201)
	for i, project := range []string{"project-a", "project-b", "project-a"} {
		input := manualTaskInput()
		input["projectId"] = project
		status, body := taskRequest(t, f, fmt.Sprintf("list-%d", i), input, nil)
		requireTask(t, status, body, 201)
	}
	if status, body := requestCancellation(t, f, "task-003"); status != 204 {
		t.Fatalf("cancel status=%d body=%s", status, body)
	}
	if _, err := f.db.Exec(`UPDATE executions SET pr_url = 'https://example.com/pr/1' WHERE task_id = 'task-002'`); err != nil {
		t.Fatal(err)
	}

	all := requireTaskList(t, f, "")
	if got := fmt.Sprint(taskIDs(all)); got != "[task-003 task-002 task-001]" || all.NextCursor != "" {
		t.Fatalf("all=%s cursor=%q", got, all.NextCursor)
	}
	first := all.Items[2]
	if first.LatestExecution == nil || first.LatestExecution.Seq != 1 || first.LatestExecution.Status != "pending" || first.PRURL != nil {
		t.Fatalf("task-001 summary=%+v", first)
	}
	if pr := all.Items[1].PRURL; pr == nil || *pr != "https://example.com/pr/1" {
		t.Fatalf("task-002 prUrl=%v", pr)
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
	for _, query := range []string{"?status=unknown", "?projectId=Bad_ID", "?status=queued&status=failed", "?limit=0", "?cursor=bad"} {
		if status, body := listTaskRequest(t, f, query); status != 400 {
			t.Fatalf("%s status=%d body=%s", query, status, body)
		}
	}
}
