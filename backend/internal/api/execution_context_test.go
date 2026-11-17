package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type selectedGlobalContext struct {
	files []GlobalContextFile
	err   error
	calls int
}

func (s *selectedGlobalContext) ReadGlobalContext(context.Context) ([]GlobalContextFile, error) {
	s.calls++
	return s.files, s.err
}

// Exercise the registered route and its authentication without changing a live
// test server's Handler. Selection is the Context Builder's separate boundary.
func requestContext(t *testing.T, f *authFixture, token, id string, source GlobalContextSource, input any) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", f.server.URL+"/api/executions/"+id+"/context", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	NewHandlerWithContextSource(f.db, nil, f.auth, nil, ArtifactConfig{}, f.settings, source, f.adapters).ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func contextInput(t *testing.T, f *authFixture, id string) map[string]any {
	t.Helper()
	setDefinition(t, f, id, "$.stages.work.instructions", `["work/instructions.md"]`)
	setDefinition(t, f, id, "$.stages.work.skills", `["work/skills"]`)
	setDefinition(t, f, id, "$.stages.work.settings", `["work/settings.json"]`)
	var commit string
	if err := f.db.QueryRow(`SELECT profile_commit_sha FROM executions WHERE id=?`, id).Scan(&commit); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"profileCommitSha": commit, "files": []contextFile{
		{"instructions", "work/instructions.md", strings.Repeat("a", 64)},
		{"skill", "work/skills/test/SKILL.md", strings.Repeat("b", 64)},
		{"settings", "work/settings.json", strings.Repeat("c", 64)},
	}}
}

type savedContext struct {
	ID, Layer, Kind, Path, Revision, Hash string
	Content                               sql.NullString
}

func savedContexts(t *testing.T, f *authFixture, id string) []savedContext {
	t.Helper()
	rows, err := f.db.Query(`SELECT id,layer,kind,source_path,source_revision,content_hash,content
		FROM context_revisions WHERE execution_id=? ORDER BY layer,kind,source_path`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := []savedContext{}
	for rows.Next() {
		var pin savedContext
		if err := rows.Scan(&pin.ID, &pin.Layer, &pin.Kind, &pin.Path, &pin.Revision, &pin.Hash, &pin.Content); err != nil {
			t.Fatal(err)
		}
		result = append(result, pin)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestContextAPIPersistsLayersAndPrepareReports(t *testing.T) {
	for _, prepare := range []string{"absent", "omitted", "empty", "reported"} {
		t.Run(prepare, func(t *testing.T) {
			f, token, id, taskID := reportFixture(t)
			input := contextInput(t, f, id)
			if prepare != "absent" {
				setDefinition(t, f, id, "$.stages.prepare", `{"command":["prepare"],"timeoutSec":60}`)
				attempt := startStage(t, f, token, id, "prepare")
				extra := map[string]any{}
				if prepare == "empty" {
					extra["reportedContext"] = []any{}
				}
				if prepare == "reported" {
					extra["reportedContext"] = []any{
						map[string]any{"source": "repo", "path": "AGENTS.md", "revision": "reported-revision", "sha256": strings.Repeat("d", 64)},
						map[string]any{"source": "workspace", "path": "CLAUDE.md", "revision": "other-revision", "sha256": strings.Repeat("e", 64)},
					}
				}
				report := finished("prepare", attempt, programResult("prepare", "succeeded", extra))
				finishStage(t, f, token, id, report)
				// Retried delivery of the prepare result must not duplicate reports.
				finishStage(t, f, token, id, report)
			}
			startStage(t, f, token, id, "work")
			source := &selectedGlobalContext{files: []GlobalContextFile{
				{Path: "context/global/policy.md", Revision: "global-version", Content: []byte("共通のルール\r\n")},
				{Path: "context/global/empty.md", Revision: "global-version", Content: []byte{}},
			}}
			status, body := requestContext(t, f, token, id, source, input)
			var reply executionContextReply
			if status != 200 || json.Unmarshal(body, &reply) != nil {
				t.Fatalf("context: %d %s", status, body)
			}
			pins := savedContexts(t, f, id)
			layers := map[string]int{}
			ids := []string{}
			for _, pin := range pins {
				layers[pin.Layer]++
				if pin.Layer != "reported" {
					ids = append(ids, pin.ID)
				}
				switch pin.Layer {
				case "global":
					var content string
					if pin.Path == "context/global/policy.md" {
						content = "共通のルール\r\n"
					} else if pin.Path != "context/global/empty.md" {
						t.Fatalf("unexpected Global path: %+v", pin)
					}
					if pin.Kind != "global_policy" || pin.Revision != "global-version" || !pin.Content.Valid || pin.Content.String != content || pin.Hash != contextHash([]byte(content)) {
						t.Fatalf("Global pin: %+v", pin)
					}
				case "harness":
					want := map[string]contextFile{}
					for _, file := range input["files"].([]contextFile) {
						want[file.Kind] = file
					}
					file, ok := want[pin.Kind]
					if !ok || pin.Path != file.Path || pin.Hash != file.SHA256 || pin.Revision != input["profileCommitSha"] || pin.Content.Valid {
						t.Fatalf("Harness pin: %+v", pin)
					}
				case "task":
					var title, body, acceptance string
					if err := f.db.QueryRow(`SELECT title,body,acceptance_criteria FROM tasks WHERE id=?`, taskID).Scan(&title, &body, &acceptance); err != nil {
						t.Fatal(err)
					}
					want, _ := json.Marshal(map[string]string{"title": title, "body": body, "acceptanceCriteria": acceptance})
					var got map[string]string
					if pin.Kind != "task_input" || !pin.Content.Valid || pin.Path != taskID || json.Unmarshal([]byte(pin.Content.String), &got) != nil || pin.Hash != contextHash([]byte(pin.Content.String)) {
						t.Fatalf("Task pin: %+v", pin)
					}
					gotJSON, _ := json.Marshal(got)
					if string(gotJSON) != string(want) {
						t.Fatalf("Task input=%s; want %s", gotJSON, want)
					}
				case "reported":
					want := map[string][2]string{"repo:AGENTS.md": {"reported-revision", strings.Repeat("d", 64)}, "workspace:CLAUDE.md": {"other-revision", strings.Repeat("e", 64)}}
					metadata, ok := want[pin.Path]
					if !ok || pin.Kind != "reported" || pin.Revision != metadata[0] || pin.Hash != metadata[1] || pin.Content.Valid {
						t.Fatalf("Reported pin: %+v", pin)
					}
				default:
					t.Fatalf("unexpected layer: %+v", pin)
				}
			}
			wantLayers := map[string]int{"global": 2, "harness": 3, "task": 1}
			if prepare == "reported" {
				wantLayers["reported"] = 2
			}
			if !reflect.DeepEqual(layers, wantLayers) {
				t.Fatalf("layers=%v; want %v", layers, wantLayers)
			}
			sort.Strings(ids)
			sort.Strings(reply.RevisionIDs)
			if !reflect.DeepEqual(ids, reply.RevisionIDs) || len(reply.Records) != 0 || source.calls != 1 {
				t.Fatalf("reply=%+v; revision IDs=%v; source reads=%d", reply, ids, source.calls)
			}
			assertContextEvent(t, f, id, pins)
		})
	}
}

func contextHash(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func assertContextEvent(t *testing.T, f *authFixture, id string, pins []savedContext) {
	t.Helper()
	if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='context_pinned'`, id); n != 1 {
		t.Fatalf("context_pinned count=%d", n)
	}
	var raw string
	if err := f.db.QueryRow(`SELECT payload FROM execution_events WHERE execution_id=? AND type='context_pinned'`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Revisions []struct{ Layer, Path, Hash string }
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	got, want := map[string]string{}, map[string]string{}
	for _, pin := range payload.Revisions {
		got[pin.Layer+":"+pin.Path] = pin.Hash
	}
	for _, pin := range pins {
		if pin.Layer != "reported" {
			want[pin.Layer+":"+pin.Path] = pin.Hash
		}
	}
	if !reflect.DeepEqual(got, want) || len(payload.Revisions) != len(want) {
		t.Fatalf("event=%s; want %v", raw, want)
	}
}

func TestContextAPIReusesOriginalPins(t *testing.T) {
	f, token, id, taskID := reportFixture(t)
	input := contextInput(t, f, id)
	startStage(t, f, token, id, "work")
	source := &selectedGlobalContext{files: []GlobalContextFile{{"context/global/policy.md", "first", []byte("first policy")}}}
	status, first := requestContext(t, f, token, id, source, input)
	if status != 200 {
		t.Fatalf("first pin: %d %s", status, first)
	}
	pins := savedContexts(t, f, id)
	execSQL(t, f, `UPDATE tasks SET title='changed',body='changed',acceptance_criteria='changed' WHERE id=?`, taskID)
	source.files = []GlobalContextFile{{"context/global/other.md", "second", []byte("changed policy")}}
	source.err = errors.New("Global source no longer readable")
	input["files"].([]contextFile)[0].SHA256 = strings.Repeat("f", 64)
	status, second := requestContext(t, f, token, id, source, input)
	if status != 200 || string(first) != string(second) {
		t.Fatalf("second pin: %d %s; first=%s", status, second, first)
	}
	if after := savedContexts(t, f, id); !reflect.DeepEqual(after, pins) || source.calls != 1 {
		t.Fatalf("pins changed or source re-read: before=%+v after=%+v calls=%d", pins, after, source.calls)
	}
	assertContextEvent(t, f, id, pins)
}

func TestContextAPIAndDatabaseRejectProjectLayer(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	input := contextInput(t, f, id)
	startStage(t, f, token, id, "work")
	for _, field := range []string{"layer", "project"} {
		input[field] = "project"
		status, body := requestContext(t, f, token, id, nil, input)
		assertAuthError(t, status, body, http.StatusBadRequest, ValidationFailed)
		delete(input, field)
	}
	input["files"].([]contextFile)[0].Kind = "project"
	status, body := requestContext(t, f, token, id, nil, input)
	assertAuthError(t, status, body, http.StatusBadRequest, ValidationFailed)
	if _, err := f.db.Exec(`INSERT INTO context_revisions(id,execution_id,layer,kind,source_path,source_revision,content_hash,content,created_at)
		VALUES ('project-pin',?,'project','instructions','AGENTS.md','revision','hash',NULL,'now')`, id); err == nil {
		t.Fatal("database allowed layer=project")
	}
	if pins := savedContexts(t, f, id); len(pins) != 0 {
		t.Fatalf("rejected inputs wrote rows: %+v", pins)
	}
}

func TestContextAPIRollsBackFailedPins(t *testing.T) {
	for _, failure := range []string{"read", "duplicate", "event"} {
		t.Run(failure, func(t *testing.T) {
			f, token, id, _ := reportFixture(t)
			input := contextInput(t, f, id)
			startStage(t, f, token, id, "work")
			source := &selectedGlobalContext{files: []GlobalContextFile{{"context/global/policy.md", "version", []byte("policy")}}}
			switch failure {
			case "read":
				source.err = errors.New("cannot read Global Context")
			case "duplicate":
				source.files = append(source.files, source.files[0])
			case "event":
				execSQL(t, f, `CREATE TRIGGER fail_context_event BEFORE INSERT ON execution_events WHEN NEW.type='context_pinned' BEGIN SELECT RAISE(ABORT,'test failure'); END`)
			}
			status, body := requestContext(t, f, token, id, source, input)
			assertAuthError(t, status, body, http.StatusInternalServerError, Internal)
			if pins := savedContexts(t, f, id); len(pins) != 0 {
				t.Fatalf("failed request left pins: %+v", pins)
			}
			if n := count(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='context_pinned'`, id); n != 0 {
				t.Fatalf("failed request left events: %d", n)
			}
		})
	}
}
