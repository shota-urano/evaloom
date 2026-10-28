package api

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const artifactID = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
const artifactPath = "/api/executions/" + streamExecution + "/artifacts"
const artifactQuery = "?artifactId=" + artifactID + "&kind=harness_log&redacted=0"
const artifactDownloadPath = artifactPath + "/" + artifactID

func uploadDownloadArtifact(t *testing.T, f *authFixture, token, content string) {
	t.Helper()
	status, body := postArtifact(t, f, token, artifactPath+artifactQuery, content)
	if status != http.StatusNoContent {
		t.Fatalf("upload status=%d body=%s", status, body)
	}
}

func TestGetArtifactReturnsUploadedBytes(t *testing.T) {
	for name, content := range map[string]string{
		"empty": "", "text": "diff\n日本語\r\n[REDACTED]\n", "binary": "\x00\xff\x01\x80\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			f, token, _ := artifactFixture(t)
			uploadDownloadArtifact(t, f, token, content)
			// Use the production HTTP router and the cookie from a real login.
			r, err := http.NewRequest("GET", f.server.URL+artifactDownloadPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			r.AddCookie(f.cookie)
			response, err := f.server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(body, []byte(content)) {
				t.Fatalf("download status=%d body=%q want=%q err=%v", response.StatusCode, body, content, err)
			}
			if response.Header.Get("Content-Type") != "application/octet-stream" ||
				response.Header.Get("Content-Length") != strconv.Itoa(len(content)) ||
				response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("download headers=%v", response.Header)
			}
		})
	}
}

func TestGetArtifactOtherExecutionReturns404(t *testing.T) {
	f, token, _ := artifactFixture(t)
	uploadDownloadArtifact(t, f, token, "only this execution")
	status, body := f.request(t, "GET", "/api/executions/"+otherExecution+"/artifacts/"+artifactID, true, "", nil)
	assertAuthError(t, status, body, 404, NotFound)
}

func TestGetArtifactWithoutLoginReturns401(t *testing.T) {
	f, token, _ := artifactFixture(t)
	uploadDownloadArtifact(t, f, token, "private artifact")
	status, body := f.request(t, "GET", artifactDownloadPath, false, "", nil)
	assertAuthError(t, status, body, 401, Unauthenticated)
	f.assertAudit(t, "GET", artifactDownloadPath, "invalid_session", 401, Unauthenticated)
}

func TestGetArtifactRunnerTokenReturns403(t *testing.T) {
	f, token, _ := artifactFixture(t)
	uploadDownloadArtifact(t, f, token, "owner only")
	status, body := f.request(t, "GET", artifactDownloadPath, false, "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	})
	assertAuthError(t, status, body, 403, Forbidden)
}

func TestGetArtifactMissingReturns404(t *testing.T) {
	f, _, _ := artifactFixture(t)
	for _, path := range []string{artifactDownloadPath, "/api/executions/" + artifactID + "/artifacts/" + artifactID} {
		status, body := f.request(t, "GET", path, true, "", nil)
		assertAuthError(t, status, body, 404, NotFound)
	}
}

func TestGetArtifactInvalidIDsReturn400(t *testing.T) {
	f, _, _ := artifactFixture(t)
	for _, path := range []string{artifactPath + "/invalid", "/api/executions/invalid/artifacts/" + artifactID} {
		status, body := f.request(t, "GET", path, true, "", nil)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
}

func TestGetArtifactStorageFailureReturnsJSON500(t *testing.T) {
	t.Run("unconfigured", func(t *testing.T) {
		f := newAuthFixture(t)
		status, body := f.request(t, "GET", artifactDownloadPath, true, "", nil)
		assertAuthError(t, status, body, 500, Internal)
	})
	t.Run("unreadable path", func(t *testing.T) {
		f, token, _ := artifactFixture(t)
		uploadDownloadArtifact(t, f, token, "content")
		if _, err := f.db.Exec(`UPDATE artifacts SET storage_path='../outside' WHERE id=?`, artifactID); err != nil {
			t.Fatal(err)
		}
		status, body := f.request(t, "GET", artifactDownloadPath, true, "", nil)
		assertAuthError(t, status, body, 500, Internal)
		if bytes.Contains(body, []byte("outside")) {
			t.Fatalf("storage path leaked: %s", body)
		}
	})
}

func artifactFixture(t *testing.T) (*authFixture, string, string) {
	t.Helper()
	dir := t.TempDir()
	f := newConfiguredAuthFixture(t, func(db *sql.DB) ArtifactConfig {
		files, err := store.NewArtifactStore(db, dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { files.Close() })
		return ArtifactConfig{Store: files, MaxBytes: 1024}
	})
	seedStreamExecutions(t, f)
	token := registerAuthRunner(t, f)
	if _, err := f.db.Exec(`UPDATE executions SET runner_id='mac-mini', status='failed' WHERE id=?`, streamExecution); err != nil {
		t.Fatal(err)
	}
	return f, token, dir
}

func postArtifact(t *testing.T, f *authFixture, token, path, content string) (int, []byte) {
	t.Helper()
	return f.request(t, "POST", path, false, "", func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Body = io.NopCloser(strings.NewReader(content)) // Unknown length exercises streaming limits.
	})
}

func assertArtifactCount(t *testing.T, f *authFixture, dir string, want int) {
	t.Helper()
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM artifacts`).Scan(&count); err != nil || count != want {
		t.Fatalf("artifacts count=%d want=%d err=%v", count, want, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "artifacts"))
	if err != nil || len(entries) != want {
		t.Fatalf("artifact files=%d want=%d err=%v", len(entries), want, err)
	}
}

func TestArtifactSameContentRetryDoesNotAddRows(t *testing.T) {
	f, token, dir := artifactFixture(t)
	// Concurrent requests must be serialized by the same ownership/write tx.
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body := postArtifact(t, f, token, artifactPath+artifactQuery, "original\n")
			if status != 204 {
				t.Errorf("upload status=%d body=%s", status, body)
			}
		}()
	}
	wg.Wait()
	assertArtifactCount(t, f, dir, 1)
}

func TestArtifactDifferentContentRetryReturns409(t *testing.T) {
	f, token, dir := artifactFixture(t)
	status, body := postArtifact(t, f, token, artifactPath+artifactQuery, "original\n")
	if status != 204 {
		t.Fatalf("first upload status=%d body=%s", status, body)
	}
	status, body = postArtifact(t, f, token, artifactPath+artifactQuery, "replacement\n")
	assertAuthError(t, status, body, 409, Conflict)
	assertArtifactCount(t, f, dir, 1)
	var path string
	if err := f.db.QueryRow(`SELECT storage_path FROM artifacts WHERE id=?`, artifactID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil || string(content) != "original\n" {
		t.Fatalf("original changed: %q %v", content, err)
	}
}

func TestArtifactOtherRunnerReturns403AndAudits(t *testing.T) {
	f, token, dir := artifactFixture(t)
	status, body := postArtifact(t, f, token, artifactPath+artifactQuery, "original")
	if status != 204 {
		t.Fatalf("first upload status=%d body=%s", status, body)
	}
	other, err := runnerauth.Register(context.Background(), f.db, "other", "Other", "/worktrees/other")
	if err != nil {
		t.Fatal(err)
	}
	// Even an identical retry must not bypass ownership or audit.
	status, body = postArtifact(t, f, other, artifactPath+artifactQuery, "original")
	assertAuthError(t, status, body, 403, Forbidden)
	f.assertAudit(t, "POST", artifactPath, "execution_runner_mismatch", 403, Forbidden)
	assertArtifactCount(t, f, dir, 1)
}

func TestArtifactRedactedOneIsStored(t *testing.T) {
	f, token, _ := artifactFixture(t)
	status, body := postArtifact(t, f, token, artifactPath+strings.ReplaceAll(artifactQuery, "redacted=0", "redacted=1"), "[REDACTED]\n")
	if status != 204 {
		t.Fatalf("upload status=%d body=%s", status, body)
	}
	var redacted int
	if err := f.db.QueryRow(`SELECT redacted FROM artifacts WHERE id=?`, artifactID).Scan(&redacted); err != nil || redacted != 1 {
		t.Fatalf("artifacts.redacted=%d err=%v", redacted, err)
	}
}

func TestArtifactRejectsInvalidInputWithoutWrites(t *testing.T) {
	f, token, dir := artifactFixture(t)
	for _, query := range []string{
		"", strings.ReplaceAll(artifactQuery, artifactID, "../unsafe"),
		strings.ReplaceAll(artifactQuery, "harness_log", "unknown"),
		strings.ReplaceAll(artifactQuery, "redacted=0", "redacted=2"),
		artifactQuery + "&artifactId=" + artifactID,
		artifactQuery + "&redacted=1", artifactQuery + "&kind=diff", "?invalid=%zz",
	} {
		status, body := postArtifact(t, f, token, artifactPath+query, "content")
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	status, body := postArtifact(t, f, token, artifactPath+artifactQuery, strings.Repeat("x", 1025))
	assertAuthError(t, status, body, 400, ValidationFailed)
	assertArtifactCount(t, f, dir, 0)
	// Exactly the configured limit is valid.
	status, body = postArtifact(t, f, token, artifactPath+artifactQuery, strings.Repeat("x", 1024))
	if status != 204 {
		t.Fatalf("at limit status=%d body=%s", status, body)
	}
}

func TestArtifactAcceptsSpecifiedKindsAndEmptyFiles(t *testing.T) {
	for _, kind := range []string{"harness_log", "test_log", "setup_log", "diff", "test_report", "test_ids", "harness_output"} {
		t.Run(kind, func(t *testing.T) {
			f, token, dir := artifactFixture(t)
			status, body := postArtifact(t, f, token, artifactPath+strings.ReplaceAll(artifactQuery, "harness_log", kind), "")
			if status != 204 {
				t.Fatalf("empty upload status=%d body=%s", status, body)
			}
			assertArtifactCount(t, f, dir, 1)
			var storedKind string
			var size, redacted int
			if err := f.db.QueryRow(`SELECT kind,size_bytes,redacted FROM artifacts WHERE id=?`, artifactID).Scan(&storedKind, &size, &redacted); err != nil || storedKind != kind || size != 0 || redacted != 0 {
				t.Fatalf("kind=%s size=%d redacted=%d err=%v", storedKind, size, redacted, err)
			}
		})
	}
}

func TestArtifactIDCannotBeReusedAcrossExecutions(t *testing.T) {
	f, token, dir := artifactFixture(t)
	status, body := postArtifact(t, f, token, artifactPath+artifactQuery, "content")
	if status != 204 {
		t.Fatalf("upload status=%d body=%s", status, body)
	}
	if _, err := f.db.Exec(`UPDATE executions SET runner_id='mac-mini' WHERE id=?`, otherExecution); err != nil {
		t.Fatal(err)
	}
	status, body = postArtifact(t, f, token, "/api/executions/"+otherExecution+"/artifacts"+artifactQuery, "content")
	assertAuthError(t, status, body, 409, Conflict)
	assertArtifactCount(t, f, dir, 1)
}

func TestArtifactDatabaseFailureRemovesNewFile(t *testing.T) {
	f, token, dir := artifactFixture(t)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_artifact BEFORE INSERT ON artifacts BEGIN SELECT RAISE(ABORT, 'artifact unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	status, body := postArtifact(t, f, token, artifactPath+artifactQuery, "content")
	assertAuthError(t, status, body, 500, Internal)
	assertArtifactCount(t, f, dir, 0)
}
