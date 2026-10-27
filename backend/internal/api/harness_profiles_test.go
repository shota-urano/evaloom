package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const validProfileYAML = `profileVersion: "1.2.0"
harnessVersion: "1.2.0"
modelId: "test-model"
promptVersion: "2025-xx"
skillVersions:
  - { name: "test-skill", version: "first" }
toolConfiguration:
  allowedTools: ["read", "write"]
contextPolicyVersion: "1"
executionConfiguration:
  adapter: "codex"
  args: ["--example", "value"]
collaborationPolicy: "solo"
evaluationPolicyVersion: null
`

func profileFixture(t *testing.T) (*authFixture, string) {
	t.Helper()
	f := harnessFixture(t)
	input := harnessInput(t, "harness-a")
	status, body := projectRequest(t, f, "POST", "/api/harnesses", input)
	h := requireHarness(t, status, body, 201)
	return f, h.RepoPath
}

func commitProfile(t *testing.T, repo, version, definition string, annotated bool) string {
	t.Helper()
	dir := filepath.Join(repo, "evaloom")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(definition), 0600); err != nil {
		t.Fatal(err)
	}
	projectGit(t, repo, "add", "evaloom/profile.yaml")
	projectGit(t, repo, "commit", "--quiet", "--allow-empty", "-m", "profile")
	args := []string{"tag", "-f"}
	if annotated {
		args = append(args, "-a", "-m", "profile")
	}
	projectGit(t, repo, append(args, "profile/v"+version)...)
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func postProfile(t *testing.T, f *authFixture, version string) (int, []byte) {
	t.Helper()
	return projectRequest(t, f, "POST", "/api/harness-profiles", map[string]any{"harnessId": "harness-a", "version": version})
}

func requireProfile(t *testing.T, status int, body []byte, want int) model.HarnessProfile {
	t.Helper()
	var p model.HarnessProfile
	if status != want {
		t.Fatalf("status=%d body=%s; want %d", status, body, want)
	}
	if err := json.Unmarshal(body, &p); err != nil || ValidateID("id", p.ID, ULID) != nil {
		t.Fatalf("invalid Profile: %s err=%v", body, err)
	}
	if _, err := ParseTimestamp(p.RegisteredAt); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHarnessProfilesRejectInvalidSemVer(t *testing.T) {
	f, repo := profileFixture(t)
	commitProfile(t, repo, "1.2.0", validProfileYAML, false)
	for _, version := range []string{"", "latest", "v1.2.0", "1.2", "01.2.0", "1.02.0", "1.2.03", "1.2.0-01", "1.2.0-alpha.01", "1.2.0-", "1.2.0+", "1.2.0-alpha..1", "1.2.0 ", "--help", "--output=/tmp/profile", "1.2.0^{tree}"} {
		t.Run(version, func(t *testing.T) {
			status, body := postProfile(t, f, version)
			requireProjectError(t, status, body, 422, PreconditionFailed, "version", "invalid_format")
			if projectCount(t, f, "harness_profiles") != 0 || projectCount(t, f, "audit_logs") != 1 {
				t.Fatal("invalid version was persisted")
			}
		})
	}
}

func TestHarnessProfilesRejectDefinitionVersionMismatch(t *testing.T) {
	f, repo := profileFixture(t)
	commitProfile(t, repo, "1.2.0", strings.Replace(validProfileYAML, `profileVersion: "1.2.0"`, `profileVersion: "1.2.1"`, 1), false)
	status, body := postProfile(t, f, "1.2.0")
	requireProjectError(t, status, body, 422, PreconditionFailed, "profileVersion", "invalid")
	if projectCount(t, f, "harness_profiles") != 0 || projectCount(t, f, "audit_logs") != 1 {
		t.Fatal("mismatched definition was persisted")
	}
}

func TestHarnessProfilesRejectAdapterMismatch(t *testing.T) {
	f, repo := profileFixture(t)
	commitProfile(t, repo, "1.2.0", strings.Replace(validProfileYAML, `adapter: "codex"`, `adapter: "claude_code"`, 1), false)
	status, body := postProfile(t, f, "1.2.0")
	requireProjectError(t, status, body, 422, PreconditionFailed, "executionConfiguration.adapter", "invalid")
	if projectCount(t, f, "harness_profiles") != 0 || projectCount(t, f, "audit_logs") != 1 {
		t.Fatal("mismatched adapter was persisted")
	}
}

func TestHarnessProfilesRejectDefinitionCaseAliases(t *testing.T) {
	cases := []struct{ name, definition string }{
		{
			"profile version mismatch hidden by alias",
			strings.Replace(validProfileYAML, `profileVersion: "1.2.0"`, `profileVersion: "9.9.9"`, 1) + "profileversion: \"1.2.0\"\n",
		},
		{
			"adapter mismatch hidden by configuration alias",
			strings.Replace(validProfileYAML, `adapter: "codex"`, `adapter: "claude_code"`, 1) + "executionconfiguration:\n  adapter: codex\n  args: []\n",
		},
		{
			"nested alias alongside adapter mismatch",
			strings.Replace(validProfileYAML, `adapter: "codex"`, "Adapter: codex\n  adapter: claude_code", 1),
		},
	}
	// Check every object level, including objects inside the skill array.
	for _, key := range []string{
		"profileVersion", "harnessVersion", "modelId", "promptVersion", "skillVersions",
		"toolConfiguration", "allowedTools", "contextPolicyVersion", "executionConfiguration",
		"adapter", "args", "collaborationPolicy", "evaluationPolicyVersion", "name", "version",
	} {
		cases = append(cases, struct{ name, definition string }{
			"incorrect case for " + key,
			strings.Replace(validProfileYAML, key+":", strings.ToUpper(key)+":", 1),
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, repo := profileFixture(t)
			commitProfile(t, repo, "1.2.0", tc.definition, false)
			status, body := postProfile(t, f, "1.2.0")
			requireProjectError(t, status, body, 422, PreconditionFailed, "definition", "invalid_format")
			if projectCount(t, f, "harness_profiles") != 0 || projectCount(t, f, "audit_logs") != 1 {
				t.Fatal("definition with case alias was persisted")
			}
		})
	}
}

func TestHarnessProfilesRegisterProvisionalFromTaggedCommit(t *testing.T) {
	for _, annotated := range []bool{false, true} {
		name := "lightweight"
		if annotated {
			name = "annotated"
		}
		t.Run(name, func(t *testing.T) {
			f, repo := profileFixture(t)
			sha := commitProfile(t, repo, "1.2.0", validProfileYAML, annotated)
			// Both a later HEAD and dirty files must be ignored during registration.
			commitProfile(t, repo, "2.0.0", strings.ReplaceAll(validProfileYAML, "1.2.0", "2.0.0"), false)
			if err := os.WriteFile(filepath.Join(repo, "evaloom/profile.yaml"), []byte("invalid: working-tree"), 0600); err != nil {
				t.Fatal(err)
			}
			status, body := postProfile(t, f, "1.2.0")
			p := requireProfile(t, status, body, 201)
			if p.HarnessID != "harness-a" || p.Version != "1.2.0" || p.CommitSHA != sha || p.TagName != "profile/v1.2.0" || p.OperationStatus != "provisional" || p.ModelID != "test-model" {
				t.Fatalf("registered=%+v", p)
			}
			var def model.ProfileDefinition
			if err := json.Unmarshal(p.Definition, &def); err != nil || def.ProfileVersion != "1.2.0" || def.ExecutionConfiguration.Adapter != "codex" || len(def.SkillVersions) != 1 || !reflect.DeepEqual(def.ExecutionConfiguration.Args, []string{"--example", "value"}) {
				t.Fatalf("definition=%s err=%v", p.Definition, err)
			}
			hash := sha256.Sum256(p.Definition)
			stored, err := store.GetHarnessProfile(context.Background(), f.db, p.HarnessID, p.Version)
			if err != nil || !reflect.DeepEqual(stored, p) || p.DefinitionHash != hex.EncodeToString(hash[:]) || projectCount(t, f, "harness_profiles") != 1 {
				t.Fatalf("stored=%+v response=%+v err=%v", stored, p, err)
			}
			var actor, targetType, targetID, detail string
			if err := f.db.QueryRow(`SELECT actor, target_type, target_id, detail FROM audit_logs WHERE action='profile.register'`).Scan(&actor, &targetType, &targetID, &detail); err != nil {
				t.Fatal(err)
			}
			if actor != "owner" || targetType != "harness_profile" || targetID != p.ID || !strings.Contains(detail, sha) || !strings.Contains(detail, p.DefinitionHash) || projectCount(t, f, "audit_logs") != 2 {
				t.Fatalf("audit=%s %s %s %s", actor, targetType, targetID, detail)
			}
		})
	}
}

func TestHarnessProfilesSameSHARegistrationReturnsExistingID(t *testing.T) {
	f, repo := profileFixture(t)
	commitProfile(t, repo, "1.2.0", validProfileYAML, false)
	status, body := postProfile(t, f, "1.2.0")
	before := requireProfile(t, status, body, 201)
	// Replacing a lightweight tag with an annotated tag at the same commit is
	// still idempotent: the identity is the commit, not the tag object's SHA.
	projectGit(t, repo, "tag", "-f", "-a", "-m", "same commit", "profile/v1.2.0")
	status, body = postProfile(t, f, "1.2.0")
	after := requireProfile(t, status, body, 200)
	if before.ID != after.ID || !reflect.DeepEqual(before, after) || projectCount(t, f, "harness_profiles") != 1 || projectCount(t, f, "audit_logs") != 2 {
		t.Fatalf("same SHA changed registration: before=%+v after=%+v", before, after)
	}
}

func TestHarnessProfilesDifferentSHAConflictPreservesRowAndAudits(t *testing.T) {
	f, repo := profileFixture(t)
	commitProfile(t, repo, "1.2.0", validProfileYAML, false)
	status, body := postProfile(t, f, "1.2.0")
	before := requireProfile(t, status, body, 201)
	// Even an invalid definition at the replacement commit must not mask the
	// tag replacement conflict or cause an update of the immutable row.
	replacement := commitProfile(t, repo, "1.2.0", "invalid: replacement", false)
	status, body = postProfile(t, f, "1.2.0")
	requireProjectError(t, status, body, 409, Conflict, "", "")
	after, err := store.GetHarnessProfile(context.Background(), f.db, "harness-a", "1.2.0")
	if err != nil || !reflect.DeepEqual(before, after) || projectCount(t, f, "harness_profiles") != 1 || projectCount(t, f, "audit_logs") != 3 {
		t.Fatalf("conflict changed registration: before=%+v after=%+v err=%v", before, after, err)
	}
	var targetID, detail string
	if err := f.db.QueryRow(`SELECT target_id, detail FROM audit_logs WHERE action='profile.register_conflict'`).Scan(&targetID, &detail); err != nil {
		t.Fatal(err)
	}
	if targetID != before.ID || !strings.Contains(detail, before.CommitSHA) || !strings.Contains(detail, replacement) {
		t.Fatalf("conflict audit=%s %s", targetID, detail)
	}
}

func TestHarnessProfilesSchemaAndMissingSourceFailClosed(t *testing.T) {
	for _, tc := range []struct{ name, definition, field, reason string }{
		{"missing tag", "", "tagName", "not_found"},
		{"missing file", "", "definition", "not_found"},
		{"malformed YAML", "profileVersion: [", "definition", "invalid_format"},
		{"missing fields", "profileVersion: 1.2.0", "definition", "invalid_format"},
		{"unknown field", validProfileYAML + "unknown: value\n", "definition", "invalid_format"},
		{"multiple documents", validProfileYAML + "---\n{}", "definition", "invalid_format"},
		{"duplicate field", validProfileYAML + "modelId: other\n", "definition", "invalid_format"},
		{"wrong scalar type", strings.Replace(validProfileYAML, `contextPolicyVersion: "1"`, "contextPolicyVersion: 1", 1), "definition", "invalid_format"},
		{"wrong array type", strings.Replace(validProfileYAML, `allowedTools: ["read", "write"]`, "allowedTools: read", 1), "definition", "invalid_format"},
		{"null args", strings.Replace(validProfileYAML, `args: ["--example", "value"]`, "args: null", 1), "definition", "invalid_format"},
		{"invalid phase policy", strings.Replace(validProfileYAML, "evaluationPolicyVersion: null", `evaluationPolicyVersion: "1"`, 1), "definition", "invalid_format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, repo := profileFixture(t)
			switch tc.name {
			case "missing tag":
			case "missing file":
				projectGit(t, repo, "tag", "profile/v1.2.0")
			default:
				commitProfile(t, repo, "1.2.0", tc.definition, false)
			}
			status, body := postProfile(t, f, "1.2.0")
			requireProjectError(t, status, body, 422, PreconditionFailed, tc.field, tc.reason)
			if projectCount(t, f, "harness_profiles") != 0 || projectCount(t, f, "audit_logs") != 1 {
				t.Fatal("invalid source was persisted")
			}
		})
	}
}

func TestHarnessProfilesConcurrentRegistrationIsIdempotent(t *testing.T) {
	f, repo := profileFixture(t)
	commitProfile(t, repo, "1.2.0", validProfileYAML, false)
	type result struct {
		status int
		body   []byte
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			status, body := postProfile(t, f, "1.2.0")
			results <- result{status, body}
		})
	}
	wg.Wait()
	close(results)
	ids, statuses := []string{}, []int{}
	for result := range results {
		if result.status != 200 && result.status != 201 {
			t.Fatalf("concurrent registration: %d %s", result.status, result.body)
		}
		p := requireProfile(t, result.status, result.body, result.status)
		ids, statuses = append(ids, p.ID), append(statuses, result.status)
	}
	sort.Ints(statuses)
	if ids[0] != ids[1] || !reflect.DeepEqual(statuses, []int{200, 201}) || projectCount(t, f, "harness_profiles") != 1 || projectCount(t, f, "audit_logs") != 2 {
		t.Fatalf("IDs=%v statuses=%v", ids, statuses)
	}
}

func TestHarnessProfilesListWithHarnessFilterAndPagination(t *testing.T) {
	f, repo := profileFixture(t)
	status, body := projectRequest(t, f, "GET", "/api/harness-profiles", nil)
	if status != 200 || string(body) != "{\"items\":[],\"nextCursor\":\"\"}\n" {
		t.Fatalf("empty list: %d %s", status, body)
	}
	want := map[string]model.HarnessProfile{}
	for _, version := range []string{"1.2.0", "1.3.0-alpha.1+build.01"} {
		commitProfile(t, repo, version, strings.ReplaceAll(validProfileYAML, "1.2.0", version), false)
		status, body := postProfile(t, f, version)
		p := requireProfile(t, status, body, 201)
		want[p.ID] = p
	}
	input := harnessInput(t, "harness-b")
	status, body = projectRequest(t, f, "POST", "/api/harnesses", input)
	h := requireHarness(t, status, body, 201)
	commitProfile(t, h.RepoPath, "1.2.0", validProfileYAML, false)
	status, body = projectRequest(t, f, "POST", "/api/harness-profiles", map[string]any{"harnessId": h.ID, "version": "1.2.0"})
	requireProfile(t, status, body, 201)
	endpoint := "/api/harness-profiles?harnessId=harness-a&limit=1"
	seen := map[string]bool{}
	for range 2 {
		status, body = projectRequest(t, f, "GET", endpoint, nil)
		var page Page[model.HarnessProfile]
		if err := json.Unmarshal(body, &page); err != nil || status != 200 || len(page.Items) != 1 {
			t.Fatalf("page: %d %s err=%v", status, body, err)
		}
		p := page.Items[0]
		if seen[p.ID] || !reflect.DeepEqual(want[p.ID], p) {
			t.Fatalf("unexpected Profile: %+v", p)
		}
		seen[p.ID] = true
		if (len(seen) == 1) != (page.NextCursor != "") {
			t.Fatalf("cursor: %s", body)
		}
		if page.NextCursor != "" {
			status, other := projectRequest(t, f, "GET", "/api/harness-profiles?harnessId=harness-b&cursor="+url.QueryEscape(page.NextCursor), nil)
			requireProjectError(t, status, other, 400, ValidationFailed, "cursor", "invalid")
		}
		endpoint = "/api/harness-profiles?harnessId=harness-a&limit=1&cursor=" + url.QueryEscape(page.NextCursor)
	}
	status, body = projectRequest(t, f, "GET", "/api/harness-profiles", nil)
	var page Page[model.HarnessProfile]
	if err := json.Unmarshal(body, &page); err != nil || status != 200 || len(page.Items) != 3 {
		t.Fatalf("unfiltered list: %d %s err=%v", status, body, err)
	}
}

func TestHarnessProfilesAuditFailureRollsBackRegistration(t *testing.T) {
	f, repo := profileFixture(t)
	commitProfile(t, repo, "1.2.0", validProfileYAML, false)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_profile_audit BEFORE INSERT ON audit_logs
		WHEN NEW.action='profile.register' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	status, body := postProfile(t, f, "1.2.0")
	requireProjectError(t, status, body, 500, Internal, "", "")
	if projectCount(t, f, "harness_profiles") != 0 || projectCount(t, f, "audit_logs") != 1 {
		t.Fatal("registration survived an audit failure")
	}
}
