package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func logReportFixture(t *testing.T) (*authFixture, string) {
	t.Helper()
	f := newAuthFixture(t)
	seedStreamExecutions(t, f)
	token := registerAuthRunner(t, f)
	if _, err := f.db.Exec(`UPDATE executions SET runner_id = 'mac-mini', status = 'failed' WHERE id = ?`, streamExecution); err != nil {
		t.Fatal(err)
	}
	return f, token
}

func postLogReport(t *testing.T, f *authFixture, token, body string) (int, []byte) {
	t.Helper()
	return f.request(t, "POST", streamPath, false, "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
		r.Body = io.NopCloser(strings.NewReader(body))
	})
}

func TestLogReportsAreAtomicAndCheckOwnership(t *testing.T) {
	f, token := logReportFixture(t)
	first := `{"events":[{"sourceId":"one","stream":"harness","text":"first"}]}`
	for range 2 {
		status, body := postLogReport(t, f, token, first)
		if status != 204 {
			t.Fatalf("status=%d body=%s", status, body)
		}
	}
	// A conflict in the second item must roll back the new first item too.
	status, body := postLogReport(t, f, token, `{"events":[{"sourceId":"two","stream":"test","text":"new"},{"sourceId":"one","stream":"harness","text":"different"}]}`)
	assertAuthError(t, status, body, 409, Conflict)
	for _, table := range []string{"execution_events"} {
		var count int
		if err := f.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	if _, err := f.db.Exec(`UPDATE executions SET runner_id = 'seed-runner' WHERE id = ?`, streamExecution); err != nil {
		t.Fatal(err)
	}
	status, body = postLogReport(t, f, token, first) // Even a duplicate needs ownership.
	assertAuthError(t, status, body, 403, Forbidden)
	f.assertAudit(t, "POST", streamPath, "execution_runner_mismatch", 403, Forbidden)
}

func TestLogReportValidationAndDatabaseFailure(t *testing.T) {
	f, token := logReportFixture(t)
	for _, body := range []string{
		`{}`, `{"events":[]}`, `{"events":null}`,
		`{"events":[{"id":"legacy","stream":"work","text":"value"}]}`,
		`{"events":[{"sourceId":"","stream":"work","text":"value"}]}`,
		`{"events":[{"sourceId":"x","stream":"unknown","text":"value"}]}`,
		`{"events":[{"sourceId":"x","stream":"test","text":"two\nlines"}]}`,
		`{"events":[{"stream":"test","text":"value"}]}`,
		`{"events":[{"sourceId":"x","stream":"test","text":"value","type":"status_changed"}]}`,
	} {
		status, response := postLogReport(t, f, token, body)
		assertAuthError(t, status, response, 400, ValidationFailed)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_event BEFORE INSERT ON execution_events WHEN json_extract(NEW.payload, '$.sourceId') = 'fail' BEGIN SELECT RAISE(FAIL, 'event failure'); END`); err != nil {
		t.Fatal(err)
	}
	status, body := postLogReport(t, f, token, `{"events":[{"sourceId":"x","stream":"runner","text":"value"},{"sourceId":"fail","stream":"runner","text":"value"}]}`)
	assertAuthError(t, status, body, 500, Internal)
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM execution_events`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial events after DB failure: %d %v", count, err)
	}
}

func TestLogReportsAcceptContractStageNames(t *testing.T) {
	f, token := logReportFixture(t)
	for _, stage := range []string{"prepare", "work", "check", "publish"} {
		status, body := postLogReport(t, f, token, `{"events":[{"sourceId":"`+stage+`","stream":"`+stage+`","text":"[REDACTED]"}]}`)
		if status != http.StatusNoContent {
			t.Fatalf("stage=%s status=%d body=%s", stage, status, body)
		}
	}
}

func TestLogSourceIDRetryChecksStreamAndText(t *testing.T) {
	f, token := logReportFixture(t)
	first := `{"events":[{"sourceId":"stage:0","stream":"work","text":"first"}]}`
	for range 2 {
		status, body := postLogReport(t, f, token, first)
		if status != http.StatusNoContent {
			t.Fatalf("retry status=%d body=%s", status, body)
		}
	}
	for _, body := range []string{
		`{"events":[{"sourceId":"stage:0","stream":"check","text":"first"}]}`,
		`{"events":[{"sourceId":"stage:0","stream":"work","text":"changed"}]}`,
	} {
		status, response := postLogReport(t, f, token, body)
		assertAuthError(t, status, response, http.StatusConflict, Conflict)
	}
	var count int
	var payload string
	if err := f.db.QueryRow(`SELECT count(*), min(payload) FROM execution_events WHERE type='log'`).Scan(&count, &payload); err != nil || count != 1 || payload != `{"sourceId":"stage:0","stream":"work","text":"first"}` {
		t.Fatalf("events=%d payload=%s err=%v", count, payload, err)
	}
}

// Upload uses the collection endpoint with query metadata, never an ID suffix.
func TestLogArtifactUploadRouteIsNotAvailable(t *testing.T) {
	f, _ := logReportFixture(t)
	status, body := f.request(t, "POST", "/api/executions/"+streamExecution+"/artifacts/01ARZ3NDEKTSV4RRFFQ69G5FAX", true, f.csrf, nil)
	assertAuthError(t, status, body, 404, NotFound)
}
