package runner

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSavedExecutionLogsReturnRedactedRunnerArtifacts(t *testing.T) {
	for _, stream := range []string{"harness", "setup", "test"} {
		t.Run(stream, func(t *testing.T) {
			f := newLogControlPlane(t)
			process, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			log, err := process.NewLog(logExecution, stream, nil, "model-secret")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { log.Finish(false) })
			if err := log.Read(strings.NewReader("日本語\r\n" + process.token + " model-secret\nAuthorization: Bearer private\n")); err != nil {
				t.Fatal(err)
			}
			if err := log.Finish(false); err != nil {
				t.Fatal(err)
			}
			if err := await(t, startLogDelivery(t, log)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE executions SET status='succeeded', finished_at='2026-10-28T00:00:00Z' WHERE id=?`, logExecution); err != nil {
				t.Fatal(err)
			}
			// Reopen both DB and file store: the GET must use durable state.
			f.stop()
			f.start()
			var savedPath string
			var redacted int
			if err := f.db.QueryRow(`SELECT storage_path, redacted FROM artifacts WHERE id=?`, log.id).Scan(&savedPath, &redacted); err != nil || redacted != 1 {
				t.Fatalf("redacted=%d err=%v", redacted, err)
			}
			saved, err := os.ReadFile(filepath.Join(f.dataDir, savedPath))
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequest("GET", f.server.URL+"/api/executions/"+logExecution+"/logs", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.AddCookie(f.cookie)
			response, err := f.server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s err=%v", response.StatusCode, body, err)
			}
			var logs map[string]string
			if err := json.Unmarshal(body, &logs); err != nil {
				t.Fatal(err)
			}
			want := "日本語\n[REDACTED] [REDACTED]\nAuthorization: [REDACTED]\n"
			if logs[stream] != want || logs[stream] != string(saved) || len(logs) != 4 {
				t.Fatalf("logs=%v saved=%q want=%q", logs, saved, want)
			}
			if strings.Contains(string(body), process.token) || strings.Contains(string(body), "model-secret") || strings.Contains(string(body), "private") {
				t.Fatalf("secret leaked: %s", body)
			}
		})
	}
}
