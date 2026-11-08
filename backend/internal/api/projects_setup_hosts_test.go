package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// spec 03 3.3.1: omitting setupAllowedHosts stores, returns, and reads back an
// empty array (setup stays fully offline).
func TestProjectsSetupAllowedHostsDefaultsToEmptyArray(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	if _, present := input["setupAllowedHosts"]; present {
		t.Fatal("fixture must omit setupAllowedHosts")
	}
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	created := requireProject(t, status, body, 201)
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["setupAllowedHosts"]) != "[]" || created.SetupAllowedHosts == nil || len(created.SetupAllowedHosts) != 0 {
		t.Fatalf("register setupAllowedHosts=%s", wire["setupAllowedHosts"])
	}
	var stored string
	if err := f.db.QueryRow(`SELECT setup_allowed_hosts FROM projects WHERE id = 'project-a'`).Scan(&stored); err != nil || stored != "[]" {
		t.Fatalf("stored=%q err=%v", stored, err)
	}
	status, body = projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	if err := json.Unmarshal(body, &wire); err != nil || status != 200 || string(wire["setupAllowedHosts"]) != "[]" {
		t.Fatalf("get status=%d setupAllowedHosts=%s err=%v", status, wire["setupAllowedHosts"], err)
	}
}

func TestProjectsSetupAllowedHostsAcceptsHostAndPort(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	hosts := []string{"registry.npmjs.org:443", "repo.packagist.org:443", "proxy.golang.org:65535", "a-1.example.com:1"}
	input["setupAllowedHosts"] = hosts
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	if p := requireProject(t, status, body, 201); !reflect.DeepEqual(p.SetupAllowedHosts, hosts) {
		t.Fatalf("setupAllowedHosts=%v; want %v", p.SetupAllowedHosts, hosts)
	}
	status, body = projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	if p := requireProject(t, status, body, 200); !reflect.DeepEqual(p.SetupAllowedHosts, hosts) {
		t.Fatalf("persisted setupAllowedHosts=%v; want %v", p.SetupAllowedHosts, hosts)
	}
}

// spec 03 3.3.1 / spec 09 3.3: wildcards, IP literals, localhost and other
// local names, and values without a port are 400 on register and update, and
// nothing is persisted or audited.
func TestProjectsSetupAllowedHostsRejectsInvalidDestinations(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	before := requireProject(t, status, body, 201)
	input["id"] = "project-b"
	for _, tc := range []struct{ name, value string }{
		{"wildcard", "*.npmjs.org:443"},
		{"wildcard-only", "*:443"},
		{"ipv4", "104.16.0.1:443"},
		{"ipv4-shorthand", "127.1:443"},
		{"ipv6", "[::1]:443"},
		{"ipv6-bare", "::1:443"},
		{"localhost", "localhost:443"},
		{"localhost-subdomain", "api.localhost:443"},
		{"docker-host", "host.docker.internal:443"},
		{"mdns", "printer.local:443"},
		{"single-label", "registry:443"},
		{"no-port", "registry.npmjs.org"},
		{"empty-port", "registry.npmjs.org:"},
		{"port-zero", "registry.npmjs.org:0"},
		{"port-too-large", "registry.npmjs.org:65536"},
		{"port-leading-zero", "registry.npmjs.org:0443"},
		{"port-non-numeric", "registry.npmjs.org:https"},
		{"uppercase", "Registry.npmjs.org:443"},
		{"scheme", "https://registry.npmjs.org:443"},
		{"path", "registry.npmjs.org:443/path"},
		{"trailing-dot", "registry.npmjs.org.:443"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{}
			for key, value := range input {
				request[key] = value
			}
			request["setupAllowedHosts"] = []string{"registry.npmjs.org:443", tc.value}
			status, body := projectRequest(t, f, "POST", "/api/projects", request)
			requireProjectError(t, status, body, 400, ValidationFailed, "setupAllowedHosts", "invalid_format")
			status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a",
				map[string]any{"setupAllowedHosts": []string{tc.value}})
			requireProjectError(t, status, body, 400, ValidationFailed, "setupAllowedHosts", "invalid_format")
		})
	}
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"setupAllowedHosts": nil})
	requireProjectError(t, status, body, 400, ValidationFailed, "setupAllowedHosts", "required")
	status, body = projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	if got := requireProject(t, status, body, 200); !reflect.DeepEqual(got, before) ||
		projectCount(t, f, "projects") != 1 || projectCount(t, f, "audit_logs") != 1 {
		t.Fatalf("rejected destinations changed project or audit history: %+v", got)
	}
}

// spec 03 3.1 step 4 and 3.3.1: an update records the values before and after
// the change in audit_logs.
func TestProjectsSetupAllowedHostsUpdateIsAudited(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	input["setupAllowedHosts"] = []string{"registry.npmjs.org:443"}
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	requireProject(t, status, body, 201)
	next := []string{"repo.packagist.org:443", "proxy.golang.org:443"}
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"setupAllowedHosts": next})
	if p := requireProject(t, status, body, 200); !reflect.DeepEqual(p.SetupAllowedHosts, next) {
		t.Fatalf("updated setupAllowedHosts=%v", p.SetupAllowedHosts)
	}
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"setupAllowedHosts": []string{}})
	if p := requireProject(t, status, body, 200); p.SetupAllowedHosts == nil || len(p.SetupAllowedHosts) != 0 {
		t.Fatalf("cleared setupAllowedHosts=%v", p.SetupAllowedHosts)
	}
	rows, err := f.db.Query(`SELECT detail FROM audit_logs WHERE action = 'project.update' AND target_id = 'project-a' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var history [][2][]string
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		var entry struct{ Before, After model.ProjectSettings }
		if err := json.Unmarshal([]byte(detail), &entry); err != nil {
			t.Fatal(err)
		}
		history = append(history, [2][]string{entry.Before.SetupAllowedHosts, entry.After.SetupAllowedHosts})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := [][2][]string{{{"registry.npmjs.org:443"}, next}, {next, {}}}
	if !reflect.DeepEqual(history, want) {
		t.Fatalf("audit setupAllowedHosts=%v; want %v", history, want)
	}
}

// spec 03 3.3.1 (Q87): a setupAllowedHosts value that lists the same host:port
// twice is 400 validation_failed with details field setupAllowedHosts on both
// register and update; the project keeps its value and audit_logs gains no row.
func TestProjectsSetupAllowedHostsRejectsDuplicateDestinations(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	original := []string{"registry.npmjs.org:443"}
	input["setupAllowedHosts"] = original
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	before := requireProject(t, status, body, 201)
	auditRows := projectCount(t, f, "audit_logs")
	var storedBefore string
	if err := f.db.QueryRow(`SELECT setup_allowed_hosts FROM projects WHERE id = 'project-a'`).Scan(&storedBefore); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		hosts []string
	}{
		{"adjacent", []string{"registry.npmjs.org:443", "registry.npmjs.org:443"}},
		{"separated", []string{"registry.npmjs.org:443", "proxy.golang.org:443", "registry.npmjs.org:443"}},
		{"new-value-twice", []string{"repo.packagist.org:443", "repo.packagist.org:443"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{}
			for key, value := range input {
				request[key] = value
			}
			request["id"] = "project-b"
			request["setupAllowedHosts"] = tc.hosts
			status, body := projectRequest(t, f, "POST", "/api/projects", request)
			requireProjectError(t, status, body, 400, ValidationFailed, "setupAllowedHosts", "duplicate")
			status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"setupAllowedHosts": tc.hosts})
			requireProjectError(t, status, body, 400, ValidationFailed, "setupAllowedHosts", "duplicate")
		})
	}
	status, body = projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	if got := requireProject(t, status, body, 200); !reflect.DeepEqual(got, before) || !reflect.DeepEqual(got.SetupAllowedHosts, original) {
		t.Fatalf("rejected duplicate changed project: %+v", got)
	}
	var storedAfter string
	if err := f.db.QueryRow(`SELECT setup_allowed_hosts FROM projects WHERE id = 'project-a'`).Scan(&storedAfter); err != nil || storedAfter != storedBefore {
		t.Fatalf("stored=%q want %q err=%v", storedAfter, storedBefore, err)
	}
	if n := projectCount(t, f, "projects"); n != 1 {
		t.Fatalf("projects=%d; rejected register must not insert", n)
	}
	if n := projectCount(t, f, "audit_logs"); n != auditRows {
		t.Fatalf("audit_logs=%d; want %d", n, auditRows)
	}
}

// spec 03 3.3.1 (Q87): the same host on different ports is not a duplicate and
// is stored as given, on register and update.
func TestProjectsSetupAllowedHostsAcceptsSameHostOnDifferentPorts(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	hosts := []string{"registry.npmjs.org:443", "registry.npmjs.org:8443"}
	input["setupAllowedHosts"] = hosts
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	if p := requireProject(t, status, body, 201); !reflect.DeepEqual(p.SetupAllowedHosts, hosts) {
		t.Fatalf("register setupAllowedHosts=%v; want %v", p.SetupAllowedHosts, hosts)
	}
	next := []string{"proxy.golang.org:443", "registry.npmjs.org:443", "proxy.golang.org:8443"}
	status, body = projectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{"setupAllowedHosts": next})
	if p := requireProject(t, status, body, 200); !reflect.DeepEqual(p.SetupAllowedHosts, next) {
		t.Fatalf("update setupAllowedHosts=%v; want %v", p.SetupAllowedHosts, next)
	}
}
