package api

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func assignmentFixture(t *testing.T) (*authFixture, string, []model.HarnessProfile) {
	t.Helper()
	f, repo := profileFixture(t)
	for _, id := range []string{"project-a", "project-b"} {
		status, body := projectRequest(t, f, "POST", "/api/projects", projectInput(t, id))
		requireProject(t, status, body, 201)
	}
	var profiles []model.HarnessProfile
	for _, version := range []string{"1.2.0", "2.0.0"} {
		commitProfile(t, repo, version, strings.ReplaceAll(validProfileYAML, "1.2.0", version), false)
		status, body := postProfile(t, f, version)
		profiles = append(profiles, requireProfile(t, status, body, 201))
	}
	return f, repo, profiles
}

func postAssignment(t *testing.T, f *authFixture, project, _, harness, version string) (int, []byte) {
	t.Helper()
	profile, err := store.GetHarnessProfile(context.Background(), f.db, harness, version)
	if err != nil {
		t.Fatal(err)
	}
	status, body := projectRequest(t, f, "PATCH", "/api/projects/"+project, map[string]any{"harnessId": harness, "profileId": profile.ID})
	if status != 200 {
		return status, body
	}
	var p model.Project
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	body, err = json.Marshal(model.ProjectHarness{ProjectID: p.ID, HarnessID: p.HarnessID, ProfileID: p.ProfileID, AssignedAt: p.UpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	return status, body
}

func requireAssignment(t *testing.T, status int, body []byte, want int) model.ProjectHarness {
	t.Helper()
	if status != want {
		t.Fatalf("status=%d body=%s; want %d", status, body, want)
	}
	var assignment model.ProjectHarness
	if err := json.Unmarshal(body, &assignment); err != nil || assignment.ProjectID == "" || assignment.HarnessID == "" || ValidateID("profileId", assignment.ProfileID, ULID) != nil {
		t.Fatalf("invalid assignment: %s err=%v", body, err)
	}
	if _, err := ParseTimestamp(assignment.AssignedAt); err != nil {
		t.Fatal(err)
	}
	return assignment
}

func storedAssignment(t *testing.T, f *authFixture, project, role string) model.ProjectHarness {
	t.Helper()
	assignment, err := store.GetProjectHarness(context.Background(), f.db, project, role)
	if err != nil {
		t.Fatal(err)
	}
	return assignment
}

func TestProjectsPinDifferentVersionsAndRejectForeignProfile(t *testing.T) {
	f, repo, profiles := assignmentFixture(t)
	for i, id := range []string{"project-a", "project-b"} {
		status, body := postAssignment(t, f, id, "", "harness-a", profiles[i].Version)
		got := requireAssignment(t, status, body, 200)
		if got.ProfileID != profiles[i].ID {
			t.Fatal(got)
		}
	}
	commitProfile(t, repo, "3.0.0", strings.ReplaceAll(validProfileYAML, "1.2.0", "3.0.0"), false)
	status, body := postProfile(t, f, "3.0.0")
	requireProfile(t, status, body, 201)
	for i, id := range []string{"project-a", "project-b"} {
		got := storedAssignment(t, f, id, "")
		if got.ProfileID != profiles[i].ID {
			t.Fatal("registration advanced pin", got)
		}
	}
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"profileId": "fixture-profile"})
	requireProjectError(t, status, body, 422, PreconditionFailed, "profileId", "invalid")
	status, body = projectRequest(t, f, "POST", "/api/projects/project-a/harnesses", map[string]any{"role": "developer", "harnessId": "harness-a", "version": "1.2.0"})
	requireProjectError(t, status, body, 404, NotFound, "", "")
}

func TestProjectWaiversRevalidatedOnProfileChange(t *testing.T) {
	f, _, profiles := assignmentFixture(t)
	if _, err := f.db.Exec(`UPDATE harness_profiles SET definition='{"approvals":[{"operation":"send","waivable":true}]}' WHERE id=?`, profiles[0].ID); err != nil {
		t.Fatal(err)
	}
	status, body := projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"harnessId": "harness-a", "profileId": profiles[0].ID, "approvalWaivers": []string{"send"}})
	requireProject(t, status, body, 200)
	before, err := store.GetProject(context.Background(), f.db, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"profileId": profiles[1].ID})
	requireProjectError(t, status, body, 422, PreconditionFailed, "approvalWaivers", "invalid")
	after, err := store.GetProject(context.Background(), f.db, "project-a")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected update changed pin: %+v err=%v", after, err)
	}
}
