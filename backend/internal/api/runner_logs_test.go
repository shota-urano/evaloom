package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/store"
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
	first := `{"events":[{"id":"one","stream":"harness","text":"first"}]}`
	for range 2 {
		status, body := postLogReport(t, f, token, first)
		if status != 204 {
			t.Fatalf("status=%d body=%s", status, body)
		}
	}
	// A conflict in the second item must roll back the new first item too.
	status, body := postLogReport(t, f, token, `{"events":[{"id":"two","stream":"test","text":"new"},{"id":"one","stream":"harness","text":"different"}]}`)
	assertAuthError(t, status, body, 409, Conflict)
	for _, table := range []string{"execution_events", "runner_event_receipts"} {
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
		`{"events":[{"id":"x","stream":"unknown","text":"value"}]}`,
		`{"events":[{"id":"x","stream":"test","text":"two\nlines"}]}`,
		`{"events":[{"stream":"test","text":"value"}]}`,
		`{"events":[{"id":"x","stream":"test","text":"value","type":"status_changed"}]}`,
	} {
		status, response := postLogReport(t, f, token, body)
		assertAuthError(t, status, response, 400, ValidationFailed)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_receipt BEFORE INSERT ON runner_event_receipts BEGIN SELECT RAISE(FAIL, 'receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	status, body := postLogReport(t, f, token, `{"events":[{"id":"x","stream":"runner","text":"value"}]}`)
	assertAuthError(t, status, body, 500, Internal)
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM execution_events`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial events after DB failure: %d %v", count, err)
	}
}

func TestLogArtifactUploadChecksOwnershipAndRetryContent(t *testing.T) {
	f, token := logReportFixture(t)
	dataDir := t.TempDir()
	artifacts, err := store.NewArtifactStore(f.db, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	server := httptest.NewServer(NewHandlerWithArtifacts(f.db, nil, f.auth, nil, ContextConfig{}, artifacts))
	defer server.Close()
	const artifactID = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	path := "/api/executions/" + streamExecution + "/artifacts/" + artifactID
	post := func(content, query string, owner bool) (int, []byte) {
		r, err := http.NewRequest("POST", server.URL+path+query, strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		r.Host = f.auth.host
		if owner {
			r.AddCookie(f.cookie)
		} else {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}
	query := "?stream=harness&redacted=true"
	for range 2 {
		if status, body := post("safe\n", query, false); status != 204 {
			t.Fatalf("upload status=%d body=%s", status, body)
		}
	}
	status, body := post("changed\n", query, false)
	assertAuthError(t, status, body, 409, Conflict)
	status, body = post("safe\n", "?stream=test&redacted=true", false)
	assertAuthError(t, status, body, 409, Conflict)
	status, body = post("safe\n", query, true)
	assertAuthError(t, status, body, 403, Forbidden)
	for _, query := range []string{"", "?stream=runner&redacted=false", "?stream=test&redacted=maybe", "?stream=test&stream=harness&redacted=false"} {
		status, body = post("safe\n", query, false)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	artifact, file, err := artifacts.Open(context.Background(), streamExecution, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(file)
	file.Close()
	if err != nil || string(content) != "safe\n" || !artifact.Redacted {
		t.Fatalf("artifact=%+v content=%q err=%v", artifact, content, err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET runner_id = 'seed-runner' WHERE id = ?`, streamExecution); err != nil {
		t.Fatal(err)
	}
	status, body = post("safe\n", query, false)
	assertAuthError(t, status, body, 403, Forbidden)
	var denials int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE target_id = ? AND json_extract(detail, '$.reason') = 'execution_runner_mismatch'`, path).Scan(&denials); err != nil || denials != 1 {
		t.Fatalf("ownership denial audit count=%d err=%v", denials, err)
	}
	files, err := os.ReadDir(filepath.Join(dataDir, "artifacts"))
	if err != nil || len(files) != 1 {
		t.Fatalf("unexpected artifact files: %v %v", files, err)
	}
}
