package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// schemaProfileFixture registers harness-a with Profile 1.2.0 whose recorded
// commit holds a params schema that $refs a second file of the same commit.
func schemaProfileFixture(t *testing.T, schemaPath string) (*authFixture, model.HarnessProfile) {
	t.Helper()
	f, repo := profileFixture(t)
	files := map[string]string{
		"evaloom/params.schema.json": `{"type":"object","properties":{"target":{"$ref":"defs.json#/$defs/target"}},"required":["target"],"additionalProperties":false}`,
		"evaloom/defs.json":          `{"$defs":{"target":{"type":"string","minLength":1}}}`,
	}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		projectGit(t, repo, "add", name)
	}
	if err := os.Symlink("params.schema.json", filepath.Join(repo, "evaloom", "link.json")); err != nil {
		t.Fatal(err)
	}
	projectGit(t, repo, "add", "evaloom/link.json")
	projectGit(t, repo, "commit", "--quiet", "-m", "params schema")
	commitProfile(t, repo, "1.2.0", validProfileYAML, false)
	status, body := postProfile(t, f, "1.2.0")
	profile := requireProfile(t, status, body, 201)
	definition, err := json.Marshal(map[string]any{"params": map[string]any{"schema": schemaPath}})
	if err != nil {
		t.Fatal(err)
	}
	stageExec(t, f, `UPDATE harness_profiles SET definition=? WHERE id=?`, string(definition), profile.ID)
	return f, profile
}

func schemaProjectInput(t *testing.T, id, profileID string, params any) map[string]any {
	input := projectInput(t, id)
	input["harnessId"], input["profileId"], input["params"] = "harness-a", profileID, params
	return input
}

func projectAudit(t *testing.T, f *authFixture, action string) (before, after *model.ProjectSettings) {
	t.Helper()
	var detail string
	if err := f.db.QueryRow(`SELECT detail FROM audit_logs WHERE action=? ORDER BY rowid DESC LIMIT 1`, action).Scan(&detail); err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	var history struct{ Before, After *model.ProjectSettings }
	if err := json.Unmarshal([]byte(detail), &history); err != nil {
		t.Fatal(err)
	}
	return history.Before, history.After
}

func TestProjectParamsFollowProfileSchema(t *testing.T) {
	f, profile := schemaProfileFixture(t, "evaloom/params.schema.json")
	for _, params := range []any{
		map[string]any{}, map[string]any{"target": 1}, map[string]any{"target": ""},
		map[string]any{"target": "x", "extra": true},
	} {
		status, body := projectRequest(t, f, "POST", "/api/projects", schemaProjectInput(t, "project-a", profile.ID, params))
		requireProjectError(t, status, body, 422, PreconditionFailed, "params", "invalid")
	}
	status, body := projectRequest(t, f, "POST", "/api/projects", schemaProjectInput(t, "project-a", profile.ID, map[string]any{"target": "x"}))
	created := requireProject(t, status, body, 201)
	if _, after := projectAudit(t, f, "project.create"); after == nil || string(after.Params) != `{"target":"x"}` {
		t.Fatalf("create audit params=%+v", after)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"params": map[string]any{"target": 2}})
	requireProjectError(t, status, body, 422, PreconditionFailed, "params", "invalid")
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"params": map[string]any{"target": "y"}})
	updated := requireProject(t, status, body, 200)
	before, after := projectAudit(t, f, "project.update")
	if before == nil || after == nil || string(before.Params) != `{"target":"x"}` || string(after.Params) != `{"target":"y"}` ||
		!reflect.DeepEqual(*before, created.ProjectSettings) || !reflect.DeepEqual(*after, updated.ProjectSettings) {
		t.Fatalf("update audit before=%+v after=%+v", before, after)
	}
	if projectCount(t, f, "projects") != 1 {
		t.Fatal("rejected params were persisted")
	}
}

func TestProjectParamsSchemaMustBeACommittedRegularFile(t *testing.T) {
	for _, schema := range []string{"evaloom/missing.json", "evaloom/link.json", "../outside.json", "/etc/passwd"} {
		t.Run(schema, func(t *testing.T) {
			f, profile := schemaProfileFixture(t, schema)
			status, body := projectRequest(t, f, "POST", "/api/projects", schemaProjectInput(t, "project-a", profile.ID, map[string]any{"target": "x"}))
			assertAuthError(t, status, body, 422, PreconditionFailed)
			if projectCount(t, f, "projects") != 0 {
				t.Fatal("project persisted with an unreadable schema")
			}
		})
	}
}

func TestProjectWithoutParamsSchemaOnlyAcceptsEmptyObject(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	input["params"] = map[string]any{"target": "x"}
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	requireProjectError(t, status, body, 422, PreconditionFailed, "params", "invalid")
	input["params"] = map[string]any{}
	status, body = projectRequest(t, f, "POST", "/api/projects", input)
	before := requireProject(t, status, body, 201)
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"params": map[string]any{"target": "x"}})
	requireProjectError(t, status, body, 422, PreconditionFailed, "params", "invalid")
	if got, err := store.GetProject(context.Background(), f.db, "project-a"); err != nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("rejected PATCH changed project: %+v err=%v", got, err)
	}
}

func TestProjectHarnessOnlyPatchIsRejected(t *testing.T) {
	f, _, _ := assignmentFixture(t)
	before, err := store.GetProject(context.Background(), f.db, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	status, body := projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"harnessId": "harness-a"})
	requireProjectError(t, status, body, 422, PreconditionFailed, "profileId", "invalid")
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"harnessId": "missing"})
	requireProjectError(t, status, body, 422, PreconditionFailed, "harnessId", "not_found")
	if after, err := store.GetProject(context.Background(), f.db, "project-a"); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected PATCH changed project: %+v err=%v", after, err)
	}
}

func TestProjectNameIDAndParamsLimits(t *testing.T) {
	f := projectFixture(t)
	for _, tc := range []struct {
		key           string
		value         any
		field, reason string
	}{
		{"name", strings.Repeat("名", 201), "name", "too_long"},
		{"id", strings.Repeat("a", 64), "id", "invalid_format"},
		{"id", "-a", "id", "invalid_format"},
		{"params", map[string]any{"v": strings.Repeat("a", 64*1024)}, "params", "too_large"},
		{"params", []any{}, "params", "invalid_format"},
	} {
		input := projectInput(t, "project-a")
		input[tc.key] = tc.value
		status, body := projectRequest(t, f, "POST", "/api/projects", input)
		requireProjectError(t, status, body, 400, ValidationFailed, tc.field, tc.reason)
	}
	for _, id := range []string{strings.Repeat("a", 63), "a--b", "a-"} {
		input := projectInput(t, id)
		input["name"] = strings.Repeat("名", 200)
		status, body := projectRequest(t, f, "POST", "/api/projects", input)
		requireProject(t, status, body, 201)
	}
}

func TestProjectListAndDetailIncludeRoutingLabel(t *testing.T) {
	f := projectFixture(t)
	for _, id := range []string{"project-a", "project-b"} {
		status, body := projectRequest(t, f, "POST", "/api/projects", projectInput(t, id))
		requireProject(t, status, body, 201)
	}
	status, body := projectRequest(t, f, "POST", "/api/routes", map[string]any{"label": "route:a", "projectId": "project-a"})
	if status != 201 {
		t.Fatalf("route=%d %s", status, body)
	}
	want := map[string]string{"project-a": `"route:a"`, "project-b": "null"}
	status, body = projectRequest(t, f, "GET", "/api/projects", nil)
	var page struct{ Items []map[string]json.RawMessage }
	if status != 200 || json.Unmarshal(body, &page) != nil || len(page.Items) != 2 {
		t.Fatalf("list=%d %s", status, body)
	}
	for _, item := range page.Items {
		var id string
		_ = json.Unmarshal(item["id"], &id)
		if label, ok := item["routingLabel"]; !ok || string(label) != want[id] {
			t.Fatalf("list %s routingLabel=%s; want %s", id, label, want[id])
		}
	}
	for id, label := range want {
		status, body = projectRequest(t, f, "GET", "/api/projects/"+id, nil)
		var detail map[string]json.RawMessage
		if status != 200 || json.Unmarshal(body, &detail) != nil || string(detail["routingLabel"]) != label {
			t.Fatalf("detail %s=%d %s", id, status, body)
		}
	}
}
