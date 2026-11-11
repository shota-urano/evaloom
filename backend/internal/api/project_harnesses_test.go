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
	setApprovals(t, f, profiles[0].ID, `[{"operation":"send","waivable":true}]`)
	status, body := projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"harnessId": "harness-a", "profileId": profiles[0].ID, "approvalWaivers": []string{"send"}})
	requireProject(t, status, body, 200)
	before, err := store.GetProject(context.Background(), f.db, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ approvals, reason string }{
		{`[{"operation":"send","waivable":false}]`, "not_waivable"},
		{`[{"operation":"send"}]`, "not_waivable"},
		{`[]`, "unknown_operation"},
	} {
		setApprovals(t, f, profiles[1].ID, tc.approvals)
		status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"profileId": profiles[1].ID})
		requireProjectError(t, status, body, 422, PreconditionFailed, "approvalWaivers", tc.reason)
		after, err := store.GetProject(context.Background(), f.db, "project-a")
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("rejected update changed pin: %+v err=%v", after, err)
		}
	}
}

func setApprovals(t *testing.T, f *authFixture, profileID, approvals string) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE harness_profiles SET definition=json_set(definition, '$.approvals', json(?)) WHERE id=?`, approvals, profileID); err != nil {
		t.Fatal(err)
	}
}

// spec 03 3.5: the same table checks Project and Harness approvalWaivers.
func TestApprovalWaiversValidation(t *testing.T) {
	f, _, profiles := assignmentFixture(t)
	setApprovals(t, f, profiles[0].ID, `[{"operation":"send","waivable":true},{"operation":"merge","waivable":false}]`)
	status, body := projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"harnessId": "harness-a", "profileId": profiles[0].ID})
	requireProject(t, status, body, 200)
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/harness-a", map[string]any{"defaultProfileId": profiles[0].ID})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	for _, path := range []string{"/api/projects/project-a", "/api/harnesses/harness-a"} {
		for _, tc := range []struct {
			name    string
			waivers any
			status  int
			code    Code
			reason  string
		}{
			{"duplicate", []string{"send", "send"}, 400, ValidationFailed, "duplicate"},
			{"not strings", []any{"send", 1}, 400, ValidationFailed, "invalid_format"},
			{"not array", "send", 400, ValidationFailed, "invalid_format"},
			{"unknown", []string{"deploy"}, 422, PreconditionFailed, "unknown_operation"},
			{"not waivable", []string{"merge"}, 422, PreconditionFailed, "not_waivable"},
		} {
			t.Run(path+" "+tc.name, func(t *testing.T) {
				status, body := projectRequest(t, f, "PATCH", path, map[string]any{"approvalWaivers": tc.waivers})
				requireProjectError(t, status, body, tc.status, tc.code, "approvalWaivers", tc.reason)
			})
		}
		status, body := projectRequest(t, f, "PATCH", path, map[string]any{"approvalWaivers": []string{"send"}})
		if status != 200 || !strings.Contains(string(body), `"approvalWaivers":["send"]`) {
			t.Fatalf("%s status=%d body=%s", path, status, body)
		}
	}
}
