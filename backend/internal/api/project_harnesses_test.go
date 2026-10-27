package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
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

func postAssignment(t *testing.T, f *authFixture, project, role, harness, version string) (int, []byte) {
	t.Helper()
	return projectRequest(t, f, "POST", "/api/projects/"+project+"/harnesses",
		map[string]any{"role": role, "harnessId": harness, "version": version})
}

func requireAssignment(t *testing.T, status int, body []byte, want int) model.ProjectHarness {
	t.Helper()
	if status != want {
		t.Fatalf("status=%d body=%s; want %d", status, body, want)
	}
	var assignment model.ProjectHarness
	if err := json.Unmarshal(body, &assignment); err != nil || assignment.ProjectID == "" || assignment.Role == "" || assignment.HarnessID == "" || ValidateID("profileId", assignment.ProfileID, ULID) != nil {
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

type assignmentAudit struct {
	Before *model.ProjectHarness `json:"before"`
	After  model.ProjectHarness  `json:"after"`
}

func assignmentAudits(t *testing.T, f *authFixture) []assignmentAudit {
	t.Helper()
	rows, err := f.db.Query(`SELECT actor, target_type, target_id, detail FROM audit_logs
		WHERE action = 'project.assign_harness' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var audits []assignmentAudit
	for rows.Next() {
		var actor, targetType, targetID, detail string
		if err := rows.Scan(&actor, &targetType, &targetID, &detail); err != nil {
			t.Fatal(err)
		}
		var audit assignmentAudit
		if err := json.Unmarshal([]byte(detail), &audit); err != nil {
			t.Fatal(err)
		}
		if actor != "owner" || targetType != "project" || targetID != audit.After.ProjectID {
			t.Fatalf("unexpected audit: %s %s %s %s", actor, targetType, targetID, detail)
		}
		audits = append(audits, audit)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return audits
}

func TestProjectHarnessesPinDifferentVersionsAcrossProjects(t *testing.T) {
	f, repo, profiles := assignmentFixture(t)
	var assignments []model.ProjectHarness
	for i, project := range []string{"project-a", "project-b"} {
		status, body := postAssignment(t, f, project, "developer", "harness-a", profiles[i].Version)
		assignment := requireAssignment(t, status, body, 201)
		if assignment.ProjectID != project || assignment.Role != "developer" || assignment.HarnessID != "harness-a" || assignment.ProfileID != profiles[i].ID {
			t.Fatalf("assignment=%+v", assignment)
		}
		assignments = append(assignments, assignment)
	}
	// A real registration through the API must not advance either Project's pin.
	commitProfile(t, repo, "3.0.0", strings.ReplaceAll(validProfileYAML, "1.2.0", "3.0.0"), false)
	status, body := postProfile(t, f, "3.0.0")
	requireProfile(t, status, body, 201)
	for _, assignment := range assignments {
		if got := storedAssignment(t, f, assignment.ProjectID, assignment.Role); !reflect.DeepEqual(got, assignment) {
			t.Fatalf("registration changed pin: got=%+v want=%+v", got, assignment)
		}
	}
	audits := assignmentAudits(t, f)
	if len(audits) != 2 || audits[0].Before != nil || audits[1].Before != nil || audits[0].After != assignments[0] || audits[1].After != assignments[1] {
		t.Fatalf("audits=%+v", audits)
	}
}

func TestProjectHarnessesReplaceOnlySelectedRoleAndAuditBeforeAfter(t *testing.T) {
	f, _, profiles := assignmentFixture(t)
	status, body := postAssignment(t, f, "project-a", "developer", "harness-a", "1.2.0")
	before := requireAssignment(t, status, body, 201)
	status, body = postAssignment(t, f, "project-a", "custom role", "harness-a", "1.2.0")
	otherRole := requireAssignment(t, status, body, 201)
	status, body = postAssignment(t, f, "project-b", "developer", "harness-a", "1.2.0")
	otherProject := requireAssignment(t, status, body, 201)
	status, body = postAssignment(t, f, "project-a", "developer", "harness-a", "2.0.0")
	after := requireAssignment(t, status, body, 200)
	if after.ProfileID != profiles[1].ID || storedAssignment(t, f, "project-a", "developer") != after ||
		storedAssignment(t, f, "project-a", "custom role") != otherRole || storedAssignment(t, f, "project-b", "developer") != otherProject || projectCount(t, f, "project_harnesses") != 3 {
		t.Fatal("replacement changed another Project or role")
	}
	audits := assignmentAudits(t, f)
	if len(audits) != 4 || audits[3].Before == nil || *audits[3].Before != before || audits[3].After != after {
		t.Fatalf("replacement audit=%+v", audits)
	}
	// Reassigning a role may also select a different Harness.
	status, body = projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-b"))
	harness := requireHarness(t, status, body, 201)
	commitProfile(t, harness.RepoPath, "1.2.0", validProfileYAML, false)
	status, body = projectRequest(t, f, "POST", "/api/harness-profiles", map[string]any{"harnessId": harness.ID, "version": "1.2.0"})
	profile := requireProfile(t, status, body, 201)
	status, body = postAssignment(t, f, "project-a", "developer", harness.ID, profile.Version)
	replaced := requireAssignment(t, status, body, 200)
	if replaced.HarnessID != harness.ID || replaced.ProfileID != profile.ID || storedAssignment(t, f, "project-a", "developer") != replaced {
		t.Fatalf("Harness replacement=%+v", replaced)
	}
	audits = assignmentAudits(t, f)
	if len(audits) != 5 || audits[4].Before == nil || *audits[4].Before != after || audits[4].After != replaced {
		t.Fatalf("Harness replacement audit=%+v", audits)
	}
}

func TestProjectHarnessesRejectUnregisteredVersionWithoutChangingPin(t *testing.T) {
	f, _, _ := assignmentFixture(t)
	status, body := postAssignment(t, f, "project-a", "developer", "harness-a", "1.2.0")
	before := requireAssignment(t, status, body, 201)
	// The requested version exists for harness-a, but never for harness-b.
	status, body = projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-b"))
	requireHarness(t, status, body, 201)
	count := projectCount(t, f, "audit_logs")
	for _, role := range []string{"developer", "new-role"} {
		for _, pair := range [][2]string{{"harness-a", "9.9.9"}, {"harness-b", "1.2.0"}, {"missing", "1.2.0"}} {
			status, body = postAssignment(t, f, "project-a", role, pair[0], pair[1])
			requireProjectError(t, status, body, 422, PreconditionFailed, "version", "not_found")
		}
	}
	if storedAssignment(t, f, "project-a", "developer") != before || projectCount(t, f, "project_harnesses") != 1 || projectCount(t, f, "audit_logs") != count {
		t.Fatal("rejected assignment changed persisted state")
	}
}

func TestProjectHarnessesAuditFailureRollsBackCreateAndReplace(t *testing.T) {
	f, _, _ := assignmentFixture(t)
	status, body := postAssignment(t, f, "project-a", "developer", "harness-a", "1.2.0")
	before := requireAssignment(t, status, body, 201)
	count := projectCount(t, f, "audit_logs")
	if _, err := f.db.Exec(`CREATE TRIGGER reject_assignment_audit BEFORE INSERT ON audit_logs
		WHEN NEW.action = 'project.assign_harness' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"project-a", "project-b"} {
		status, body = postAssignment(t, f, project, "developer", "harness-a", "2.0.0")
		requireProjectError(t, status, body, 500, Internal, "", "")
		if strings.Contains(string(body), "audit unavailable") {
			t.Fatal("internal error leaked")
		}
	}
	if storedAssignment(t, f, "project-a", "developer") != before || projectCount(t, f, "project_harnesses") != 1 || projectCount(t, f, "audit_logs") != count {
		t.Fatal("audit failure did not roll back assignment")
	}
}

func TestProjectHarnessesInvalidInputsAndMissingProject(t *testing.T) {
	f, _, _ := assignmentFixture(t)
	count := projectCount(t, f, "audit_logs")
	for _, tc := range []struct {
		project, role, harness, version string
		status                          int
		code                            Code
		field, reason                   string
	}{
		{"INVALID", "developer", "harness-a", "1.2.0", 400, ValidationFailed, "id", "invalid_format"},
		{"project-a", "", "harness-a", "1.2.0", 400, ValidationFailed, "role", "required"},
		{"project-a", " \t", "harness-a", "1.2.0", 400, ValidationFailed, "role", "required"},
		{"project-a", "developer", "", "1.2.0", 400, ValidationFailed, "harnessId", "required"},
		{"project-a", "developer", "INVALID", "1.2.0", 400, ValidationFailed, "harnessId", "invalid_format"},
		{"project-a", "developer", "harness-a", "latest", 422, PreconditionFailed, "version", "invalid_format"},
		{"project-a", "developer", "harness-a", "", 422, PreconditionFailed, "version", "invalid_format"},
		{"missing", "developer", "harness-a", "1.2.0", 404, NotFound, "", ""},
	} {
		status, body := postAssignment(t, f, tc.project, tc.role, tc.harness, tc.version)
		requireProjectError(t, status, body, tc.status, tc.code, tc.field, tc.reason)
	}
	for _, raw := range []string{`null`, `[]`, `{`, `{} {}`, `{"role":3}`, `{"role":"developer","harnessId":"harness-a","version":"1.2.0","profileId":"untrusted"}`} {
		status, body := f.request(t, "POST", "/api/projects/project-a/harnesses", true, f.csrf, func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(raw))
			r.Header.Set("Content-Type", "application/json")
		})
		requireProjectError(t, status, body, 400, ValidationFailed, "body", "invalid_json")
	}
	if projectCount(t, f, "project_harnesses") != 0 || projectCount(t, f, "audit_logs") != count {
		t.Fatal("invalid input was persisted")
	}
}

func TestProjectHarnessesAuthenticationCSRFAndRunnerSeparation(t *testing.T) {
	f, _, _ := assignmentFixture(t)
	token, err := runnerauth.Register(context.Background(), f.db, "auth-runner", "Auth Runner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const endpoint = "/api/projects/project-a/harnesses"
	status, body := f.request(t, "POST", endpoint, false, f.csrf, nil)
	requireProjectError(t, status, body, 401, Unauthenticated, "", "")
	status, body = f.request(t, "POST", endpoint, true, "", nil)
	requireProjectError(t, status, body, 403, Forbidden, "", "")
	status, body = f.request(t, "POST", endpoint, false, "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) })
	requireProjectError(t, status, body, 403, Forbidden, "", "")
	if projectCount(t, f, "project_harnesses") != 0 {
		t.Fatal("unauthorized request changed assignment")
	}
}

func TestProjectHarnessesConcurrentReplacementPreservesAuditChain(t *testing.T) {
	f, _, profiles := assignmentFixture(t)
	var wg sync.WaitGroup
	for _, profile := range profiles {
		wg.Add(1)
		go func(version string) {
			defer wg.Done()
			status, body := postAssignment(t, f, "project-a", "developer", "harness-a", version)
			if status != 200 && status != 201 {
				t.Errorf("status=%d body=%s", status, body)
			}
		}(profile.Version)
	}
	wg.Wait()
	audits := assignmentAudits(t, f)
	if len(audits) != 2 || audits[0].Before != nil || audits[1].Before == nil || *audits[1].Before != audits[0].After || audits[0].After.ProfileID == audits[1].After.ProfileID ||
		storedAssignment(t, f, "project-a", "developer") != audits[1].After || projectCount(t, f, "project_harnesses") != 1 {
		t.Fatalf("concurrent assignment history=%+v", audits)
	}
}

// The management UI shows each role's pinned Harness and version on the
// Projects list and the Project detail (spec 13, 3.2).
func TestProjectReadsIncludePinnedHarnessVersions(t *testing.T) {
	f, _, profiles := assignmentFixture(t)
	for _, a := range []struct{ role, version string }{{"reviewer", profiles[1].Version}, {"developer", profiles[0].Version}} {
		status, body := postAssignment(t, f, "project-a", a.role, "harness-a", a.version)
		requireAssignment(t, status, body, 201)
	}
	type view struct {
		ID        string                     `json:"id"`
		Harnesses []model.ProjectHarnessView `json:"harnesses"`
	}
	status, body := projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	var detail view
	if status != 200 || json.Unmarshal(body, &detail) != nil {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(detail.Harnesses) != 2 {
		t.Fatalf("harnesses=%+v", detail.Harnesses)
	}
	for i, want := range []struct {
		role    string
		profile model.HarnessProfile
	}{{"developer", profiles[0]}, {"reviewer", profiles[1]}} {
		got := detail.Harnesses[i]
		if got.ProjectID != "project-a" || got.Role != want.role || got.HarnessID != "harness-a" || got.ProfileID != want.profile.ID ||
			got.Version != want.profile.Version || got.CommitSHA != want.profile.CommitSHA {
			t.Fatalf("harnesses[%d]=%+v want role=%s profile=%+v", i, got, want.role, want.profile)
		}
	}
	status, body = projectRequest(t, f, "GET", "/api/projects", nil)
	var page struct{ Items []view }
	if status != 200 || json.Unmarshal(body, &page) != nil || len(page.Items) != 2 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if page.Items[0].ID != "project-a" || !reflect.DeepEqual(page.Items[0].Harnesses, detail.Harnesses) {
		t.Fatalf("list project-a=%+v", page.Items[0])
	}
	// A Project without pins lists an empty array, not null.
	if page.Items[1].ID != "project-b" || page.Items[1].Harnesses == nil || len(page.Items[1].Harnesses) != 0 {
		t.Fatalf("list project-b=%+v body=%s", page.Items[1], body)
	}
}
