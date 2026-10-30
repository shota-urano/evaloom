package securitytest

import (
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
)

// Test-owned destinations, not Project defaults or a registry persistence API.
const isolationRegistry = "registry.npmjs.org"

type isolationTargets struct {
	controlURLs []string
	db          string
	files       map[string]string
	sockets     []string
	worktree    string
	secret      string
}

var isolationEnv = []string{"PATH", "LC_ALL", "PYTHONDONTWRITEBYTECODE"}

func isolationPlan(stage string, targets isolationTargets) ([]map[string]any, []boundaryRule, []string) {
	var plan []map[string]any
	var rules []boundaryRule
	add := func(id, kind string, allowed bool, item map[string]any) {
		item["id"], item["kind"] = id, kind
		plan = append(plan, item)
		rules = append(rules, boundaryRule{id, kind, allowed})
	}
	for i, url := range targets.controlURLs {
		family := []string{"v4", "v6"}[i]
		for _, endpoint := range []struct{ id, method, path string }{
			{"owner", "GET", "/api/projects"}, {"runner", "POST", "/api/runners/boundary/claim"},
		} {
			add("control-"+endpoint.id+"-"+family, "http", false,
				map[string]any{"url": url + endpoint.path, "method": endpoint.method, "timeoutSec": 1})
		}
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
		portNumber, _ := strconv.Atoi(port)
		for _, host := range []string{"127.0.0.1", "::1", "host.docker.internal", "gateway.docker.internal"} {
			if stage == "setup" {
				add("control-proxy-"+family+"-"+host, "proxy_connect", false,
					map[string]any{"host": host, "port": portNumber, "timeoutSec": 2})
			}
			if strings.HasSuffix(host, ".internal") {
				add("control-direct-"+family+"-"+host, "http", false,
					map[string]any{"url": "http://" + net.JoinHostPort(host, port) + "/api/projects", "timeoutSec": 1})
			}
		}
	}
	add("db-read", "read", false, map[string]any{"path": targets.db})
	add("db-write", "write", false, map[string]any{"path": targets.db, "value": "test-owned-isolation-write"})
	// Keep ordering deterministic, including the independently held rule list.
	ids := make([]string, 0, len(targets.files))
	for id := range targets.files {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		add(id, "read", false, map[string]any{"path": targets.files[id]})
	}
	for i, path := range targets.sockets {
		add(fmt.Sprintf("management-socket-%d", i), "exists", false, map[string]any{"path": path})
	}
	add("personal-home", "exists", false, map[string]any{"path": filepath.Dir(targets.files["home-canary"])})
	add("worktree-dotenv-exists", "exists", false, map[string]any{"path": filepath.Join(targets.worktree, ".env")})
	allowObserved := append(slices.Clone(isolationEnv), "HOME")
	if stage == "setup" {
		allowObserved = append(allowObserved, "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy")
		// Successful CONNECT is a positive control for the complete egress path.
		add("registry-allowed", "proxy_connect", true, map[string]any{"host": isolationRegistry, "port": 443, "timeoutSec": 10})
		add("registry-wrong-port", "proxy_connect", false, map[string]any{"host": isolationRegistry, "port": 80, "timeoutSec": 2})
		add("external-proxy", "proxy_connect", false, map[string]any{"host": "github.com", "port": 443, "timeoutSec": 2})
		add("external-ip-proxy", "proxy_connect", false, map[string]any{"host": "1.1.1.1", "port": 443, "timeoutSec": 2})
	} else {
		// TestRun has no proxy. Do not misclassify its intentional absence as a
		// proxy policy denial: proxy_connect requires a live filtering proxy.
		add("proxy-absent", "tcp", false, map[string]any{"host": "127.0.0.1", "port": 18080, "timeoutSec": 1})
	}
	for _, target := range []struct{ id, host string }{{"registry-direct", isolationRegistry}, {"external-direct", "github.com"}, {"external-ip-direct", "1.1.1.1"}} {
		add(target.id, "tcp", false, map[string]any{"host": target.host, "port": 443, "timeoutSec": 2})
	}
	add("environment", "env", true, map[string]any{"allowlist": allowObserved, "secrets": []string{targets.secret}})
	add("worktree-positive", "read", true, map[string]any{"path": filepath.Join(targets.worktree, "app.py")})
	if stage == "setup" {
		add("worktree-output", "write", true, map[string]any{"path": filepath.Join(targets.worktree, "setup-marker"), "value": "setup-ran"})
	} else {
		add("setup-output", "read", true, map[string]any{"path": filepath.Join(targets.worktree, "setup-marker")})
	}
	return plan, rules, allowObserved
}

func TestIsolationVerdict(t *testing.T) {
	targets := isolationTargets{controlURLs: []string{"http://127.0.0.1:1234", "http://[::1]:5678"}, db: "/protected/evaloom.db",
		files: map[string]string{"runner-token": "/protected/token", "publisher": "/personal/.git-credentials",
			"publisher-config": "/personal/.gitconfig", "keychain": "/personal/keychain", "home-canary": "/personal/canary",
			"project-dotenv": "/owner/project/.env", "worktree-dotenv": "/worktree/.env", "ssh-key": "/personal/.ssh/id_ed25519",
			"logs": "/protected/log", "artifacts": "/protected/artifact", "model-config": "/dedicated/model-credentials"},
		sockets: []string{"/var/run/docker.sock", "/run/docker.sock", "/host/runner.sock"}, worktree: "/worktree", secret: "dummy"}
	for _, stage := range []string{"setup", "test"} {
		t.Run(stage, func(t *testing.T) {
			_, rules, allowEnv := isolationPlan(stage, targets)
			for _, id := range []string{"control-owner-v4", "control-owner-v6", "control-runner-v4", "control-runner-v6", "db-read", "db-write",
				"runner-token", "publisher", "publisher-config", "keychain", "ssh-key", "personal-home", "home-canary", "project-dotenv", "worktree-dotenv",
				"logs", "artifacts", "model-config",
				"management-socket-0", "management-socket-1", "management-socket-2", "registry-direct", "external-direct", "external-ip-direct"} {
				if !slices.ContainsFunc(rules, func(rule boundaryRule) bool { return rule.id == id && !rule.allowed }) {
					t.Fatalf("missing spec 3.3 denial: %s", id)
				}
			}
			if stage == "setup" && !slices.Contains(rules, boundaryRule{"registry-allowed", "proxy_connect", true}) {
				t.Fatal("setup must prove the allowed registry is reachable")
			}
			var observations []probe.Observation
			for _, rule := range rules {
				ok := rule.allowed
				detail := map[string]any{}
				if rule.kind == "proxy_connect" {
					status := 403
					if ok {
						status = 200
					}
					detail["status"] = status
				}
				if rule.kind == "env" {
					detail = map[string]any{"names": allowEnv, "unexpected": []string{}, "secretPresent": false}
				}
				observations = append(observations, probe.Observation{ID: rule.id, Kind: rule.kind, Succeeded: &ok, Detail: detail})
			}
			snapshot := boundarySnapshot{http: 4, audits: 4, refs: map[string]string{"refs/heads/main": "base"}}
			check := func(t *testing.T, obs []probe.Observation, after boundarySnapshot, want string) {
				t.Helper()
				err := boundaryVerdict(verdictReport(t, obs), rules, allowEnv, snapshot, after)
				if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
					t.Fatalf("want %q, got %v", want, err)
				}
			}
			check(t, observations, snapshot, "")
			for i, rule := range rules {
				t.Run(rule.id, func(t *testing.T) {
					obs := slices.Clone(observations)
					flipped := !rule.allowed
					obs[i].Succeeded = &flipped
					if rule.kind == "proxy_connect" {
						status := 403
						if flipped {
							status = 200
						}
						obs[i].Detail = map[string]any{"status": status}
					}
					check(t, obs, snapshot, rule.id)
					check(t, append(slices.Clone(observations[:i]), observations[i+1:]...), snapshot, rule.id)
					obs = slices.Clone(observations)
					obs[i].Kind = "wrong-kind"
					check(t, obs, snapshot, rule.id)
					if rule.kind == "env" {
						obs[i] = observations[i]
						obs[i].Detail = map[string]any{"names": append(slices.Clone(allowEnv), "EVALOOM_ISOLATION_SECRET"),
							"unexpected": []string{}, "secretPresent": true}
						check(t, obs, snapshot, "environment")
					}
					if rule.kind == "proxy_connect" {
						for _, detail := range []any{map[string]any{}, map[string]any{"status": 403, "unavailable": true}, map[string]any{"status": 502}} {
							obs[i] = observations[i]
							obs[i].Detail = detail
							check(t, obs, snapshot, "実行不能")
						}
					}
				})
			}
			for _, id := range []string{"control-http", "control-audit"} {
				after := snapshot
				if id == "control-http" {
					after.http++
				} else {
					after.audits++
				}
				check(t, observations, after, id)
			}
			for _, data := range [][]byte{nil, []byte(`{}`), []byte(`{"observations":[]}`)} {
				if err := boundaryVerdict(data, rules, allowEnv, snapshot, snapshot); err == nil {
					t.Fatal("missing/invalid report passed")
				}
			}
		})
	}
}
