package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

type recordAPIFixture struct {
	f       *authFixture
	token   string
	id      string
	handler http.Handler
	dir     string
}

func (x recordAPIFixture) get(t *testing.T, artifactID string, header func(*http.Request)) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest("GET", x.f.server.URL+"/api/executions/"+x.id+"/records/"+artifactID, nil)
	if header != nil {
		header(r)
	}
	w := httptest.NewRecorder()
	x.handler.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func (x recordAPIFixture) bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func (x recordAPIFixture) writeFile(t *testing.T, artifactID, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(x.dir, "artifacts", artifactID), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	execSQL(t, x.f, `UPDATE artifacts SET size_bytes=? WHERE id=?`, len(content), artifactID)
}

// newRecordAPIFixture pins the newest record of harness-a for a working
// Execution (records.pass.recent=1) through the real context API, and returns
// the pinned, an older unpinned, and another Harness's record.
func newRecordAPIFixture(t *testing.T) (recordAPIFixture, contextRecordSeed, contextRecordSeed, contextRecordSeed) {
	t.Helper()
	f, token, id, _ := reportFixture(t)
	input := contextInput(t, f, id)
	setDefinition(t, f, id, "$.records", `{"keep":[],"pass":{"recent":1}}`)
	sourceID, taskID := contextRecordSource(t, f)
	execSQL(t, f, `INSERT INTO harnesses(id,name,repo_url,repo_path,created_at,updated_at)
		VALUES ('other','Other','','','t','t')`)
	older := seedContextRecord(t, f, sourceID, taskID, "harness-a", 1)
	pinned := seedContextRecord(t, f, sourceID, taskID, "harness-a", 2)
	// Newer than both, but belongs to another Harness: never pinned here.
	foreign := seedContextRecord(t, f, sourceID, taskID, "other", 3)
	startStage(t, f, token, id, "work")
	status, body := requestContext(t, f, token, id, nil, input)
	assertContextRecords(t, f, id, status, body, []contextRecordSeed{pinned})
	dir := t.TempDir()
	files, err := store.NewArtifactStore(f.db, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	x := recordAPIFixture{f: f, token: token, id: id, dir: dir,
		handler: NewHandlerWithArtifacts(f.db, nil, f.auth, nil, ArtifactConfig{Store: files, MaxBytes: 1024}, f.adapters)}
	for _, record := range []contextRecordSeed{older, pinned, foreign} {
		x.writeFile(t, record.artifactID, "content of "+record.id)
	}
	return x, pinned, older, foreign
}

// Spec 07 5 / Q208: only the record artifacts pinned for this Execution are
// served; any other artifactId is 404.
func TestRecordAPIServesOnlyPinnedRecords(t *testing.T) {
	x, pinned, older, foreign := newRecordAPIFixture(t)
	status, body := x.get(t, pinned.artifactID, x.bearer(x.token))
	if status != http.StatusOK || string(body) != "content of "+pinned.id {
		t.Fatalf("pinned record: %d %q", status, body)
	}
	ownArtifact := addArtifact(t, x.f, x.id, "work", "out_file", "own.md")
	x.writeFile(t, ownArtifact, "not a record")
	for name, artifactID := range map[string]string{
		"older_record_not_pinned":    older.artifactID,
		"other_harness_record":       foreign.artifactID,
		"artifact_of_this_execution": ownArtifact,
		"unknown_artifact":           ulid.Make().String(),
	} {
		t.Run(name, func(t *testing.T) {
			status, body := x.get(t, artifactID, x.bearer(x.token))
			if status != http.StatusNotFound {
				t.Fatalf("status=%d body=%s; want 404", status, body)
			}
		})
	}
}

// Even a record pinned for one Execution cannot be read through another.
func TestRecordAPIDoesNotServeRecordsPinnedForAnotherExecution(t *testing.T) {
	x, pinned, _, _ := newRecordAPIFixture(t)
	execSQL(t, x.f, `DELETE FROM context_revisions WHERE execution_id=? AND kind='record'`, x.id)
	status, body := x.get(t, pinned.artifactID, x.bearer(x.token))
	if status != http.StatusNotFound {
		t.Fatalf("status=%d body=%s; want 404 without this Execution's pin", status, body)
	}
}

// Spec 07 5: called with Runner authentication only.
func TestRecordAPIRequiresRunnerAuthentication(t *testing.T) {
	x, pinned, _, _ := newRecordAPIFixture(t)
	for name, header := range map[string]func(*http.Request){
		"no_credentials": nil,
		"invalid_token":  x.bearer("not-a-runner-token"),
	} {
		t.Run(name, func(t *testing.T) {
			status, body := x.get(t, pinned.artifactID, header)
			if status != http.StatusUnauthorized || string(body) == "content of "+pinned.id {
				t.Fatalf("status=%d body=%s; want 401", status, body)
			}
		})
	}
	t.Run("another_runner", func(t *testing.T) {
		other, err := runnerauth.Register(t.Context(), x.f.db, "other-runner", "Other Runner", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		status, body := x.get(t, pinned.artifactID, x.bearer(other))
		if status != http.StatusForbidden || string(body) == "content of "+pinned.id {
			t.Fatalf("status=%d body=%s; want 403", status, body)
		}
	})
}

// A pinned record whose stored file is gone is 404, so the Runner skips it.
func TestRecordAPIMissingFileIsNotFound(t *testing.T) {
	x, pinned, _, _ := newRecordAPIFixture(t)
	if err := os.Remove(filepath.Join(x.dir, "artifacts", pinned.artifactID)); err != nil {
		t.Fatal(err)
	}
	status, body := x.get(t, pinned.artifactID, x.bearer(x.token))
	if status != http.StatusNotFound {
		t.Fatalf("status=%d body=%s; want 404", status, body)
	}
}
