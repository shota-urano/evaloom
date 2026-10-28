package api

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const executionLogsPath = "/api/executions/" + streamExecution + "/logs"

func requireExecutionLogs(t *testing.T, f *authFixture, path string, want map[string]string) {
	t.Helper()
	status, body := f.request(t, "GET", path, true, "", nil)
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil || status != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("logs status=%d body=%s want=%v err=%v", status, body, want, err)
	}
}

func TestExecutionLogsFinishedStreamsMatchUploadedArtifacts(t *testing.T) {
	for _, state := range []string{"succeeded", "failed", "interrupted", "cancelled", "timed_out"} {
		t.Run(state, func(t *testing.T) {
			f, token, _ := artifactFixture(t)
			if _, err := f.db.Exec(`UPDATE executions SET status=?, finished_at='2026-10-28T00:00:00Z' WHERE id=?`, state, streamExecution); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"harness": "harness\r\n日本語\n", "setup": "setup\n\n", "test": "test without final newline", "runner": "runner first\n\nrunner last\n"}
			for stream, id := range map[string]string{
				"harness": artifactID, "setup": "01ARZ3NDEKTSV4RRFFQ69G5FAY", "test": "01ARZ3NDEKTSV4RRFFQ69G5FAZ",
			} {
				status, body := postArtifact(t, f, token, artifactPath+"?artifactId="+id+"&kind="+stream+"_log&redacted=0", want[stream])
				if status != http.StatusNoContent {
					t.Fatalf("upload %s status=%d body=%s", stream, status, body)
				}
				status, body = f.request(t, "GET", artifactPath+"/"+id, true, "", nil)
				if status != http.StatusOK || string(body) != want[stream] {
					t.Fatalf("artifact %s status=%d body=%q", stream, status, body)
				}
			}
			status, body := postLogReport(t, f, token, `{"events":[
				{"id":"r1","stream":"runner","text":"runner first"},
				{"id":"r2","stream":"runner","text":""},
				{"id":"h1","stream":"harness","text":"do not duplicate artifact"},
				{"id":"s1","stream":"setup","text":"do not duplicate artifact"},
				{"id":"t1","stream":"test","text":"do not duplicate artifact"},
				{"id":"r3","stream":"runner","text":"runner last"}]}`)
			if status != http.StatusNoContent {
				t.Fatalf("events status=%d body=%s", status, body)
			}
			// Both registration and retrieval go through the production router.
			requireExecutionLogs(t, f, executionLogsPath, want)
		})
	}
}

func TestExecutionLogsExcludeOtherExecutionsAndNonLogArtifacts(t *testing.T) {
	f, token, _ := artifactFixture(t)
	status, body := postArtifact(t, f, token, artifactPath+strings.ReplaceAll(artifactQuery, "harness_log", "diff"), "not a log")
	if status != http.StatusNoContent {
		t.Fatalf("upload status=%d body=%s", status, body)
	}
	if _, err := f.db.Exec(`UPDATE executions SET runner_id='mac-mini' WHERE id=?`, otherExecution); err != nil {
		t.Fatal(err)
	}
	otherPath := "/api/executions/" + otherExecution
	status, body = postArtifact(t, f, token, otherPath+"/artifacts?artifactId=01ARZ3NDEKTSV4RRFFQ69G5FAY&kind=harness_log&redacted=0", "other execution log")
	if status != http.StatusNoContent {
		t.Fatalf("other upload status=%d body=%s", status, body)
	}
	status, body = f.request(t, "POST", otherPath+"/events", false, "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
		r.Body = io.NopCloser(strings.NewReader(`{"events":[{"id":"other","stream":"runner","text":"other runner"}]}`))
	})
	if status != http.StatusNoContent {
		t.Fatalf("other events status=%d body=%s", status, body)
	}
	requireExecutionLogs(t, f, executionLogsPath, map[string]string{"harness": "", "setup": "", "test": "", "runner": ""})
	requireExecutionLogs(t, f, otherPath+"/logs", map[string]string{"harness": "other execution log", "setup": "", "test": "", "runner": "other runner\n"})
}

func TestExecutionLogsMultipleArtifactsPreserveBytesInOrder(t *testing.T) {
	f, token, _ := artifactFixture(t)
	for _, item := range []struct{ id, text string }{
		{"01ARZ3NDEKTSV4RRFFQ69G5FAZ", "last"},
		{artifactID, "first\r\n"},
		{"01ARZ3NDEKTSV4RRFFQ69G5FAY", ""},
	} {
		status, body := postArtifact(t, f, token, artifactPath+"?artifactId="+item.id+"&kind=test_log&redacted=0", item.text)
		if status != http.StatusNoContent {
			t.Fatalf("upload status=%d body=%s", status, body)
		}
	}
	if _, err := f.db.Exec(`UPDATE artifacts SET created_at='2026-10-28T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	requireExecutionLogs(t, f, executionLogsPath, map[string]string{"harness": "", "setup": "", "test": "first\r\nlast", "runner": ""})
}

func TestExecutionLogsMissingExecutionReturns404(t *testing.T) {
	f, _, _ := artifactFixture(t)
	status, body := f.request(t, "GET", "/api/executions/"+artifactID+"/logs", true, "", nil)
	assertAuthError(t, status, body, http.StatusNotFound, NotFound)
}

func TestExecutionLogsInvalidIDReturns400(t *testing.T) {
	f, _, _ := artifactFixture(t)
	status, body := f.request(t, "GET", "/api/executions/invalid/logs", true, "", nil)
	requireProjectError(t, status, body, http.StatusBadRequest, ValidationFailed, "id", "invalid_format")
}

func TestExecutionLogsRequireOwnerAuthentication(t *testing.T) {
	f, token, _ := artifactFixture(t)
	status, body := f.request(t, "GET", executionLogsPath, false, "", nil)
	assertAuthError(t, status, body, http.StatusUnauthorized, Unauthenticated)
	f.assertAudit(t, "GET", executionLogsPath, "invalid_session", http.StatusUnauthorized, Unauthenticated)
	status, body = f.request(t, "GET", executionLogsPath, false, "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	})
	assertAuthError(t, status, body, http.StatusForbidden, Forbidden)
}

func TestExecutionLogsStorageFailureReturnsJSON500(t *testing.T) {
	for _, path := range []string{"artifacts/missing", "../outside"} {
		t.Run(path, func(t *testing.T) {
			f, token, _ := artifactFixture(t)
			uploadDownloadArtifact(t, f, token, "private log")
			if _, err := f.db.Exec(`UPDATE artifacts SET storage_path=? WHERE id=?`, path, artifactID); err != nil {
				t.Fatal(err)
			}
			status, body := f.request(t, "GET", executionLogsPath, true, "", nil)
			assertAuthError(t, status, body, http.StatusInternalServerError, Internal)
			if strings.Contains(string(body), path) || strings.Contains(string(body), "private log") {
				t.Fatalf("internal data leaked: %s", body)
			}
		})
	}
}
