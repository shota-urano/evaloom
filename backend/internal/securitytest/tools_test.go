package securitytest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter/probe"
)

type available struct{ err error }

func (a available) Check(context.Context) error { return a.err }

func TestPrerequisitesAreErrorsRatherThanSkips(t *testing.T) {
	missing := errors.New("not installed")
	for _, tc := range []struct {
		name          string
		prerequisites Prerequisites
		fail          bool
	}{
		{"sandbox_absent", Prerequisites{}, true},
		{"sandbox_unusable", Prerequisites{Sandbox: available{missing}}, true},
		{"model_absent", Prerequisites{Sandbox: available{}, RequireModel: true}, true},
		{"model_unusable", Prerequisites{Sandbox: available{}, RequireModel: true, ModelCredentials: func(context.Context) error { return missing }}, true},
		{"probe_without_model", Prerequisites{Sandbox: available{}}, false},
		{"model_ready", Prerequisites{Sandbox: available{}, RequireModel: true, ModelCredentials: func(context.Context) error { return nil }}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.prerequisites.Check(context.Background())
			if (err != nil) != tc.fail {
				t.Fatalf("error=%v fail=%v", err, tc.fail)
			}
			if tc.fail && !strings.Contains(err.Error(), "実行不能") {
				t.Fatalf("lost unavailable diagnostic: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (Prerequisites{Sandbox: available{}}).Check(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func TestNetworkProbesDistinguishConnectionsFromDenials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f, err := NewFixture(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Host {
		case "allowed.invalid:443":
			w.WriteHeader(http.StatusOK)
		case "forbidden.invalid:443":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer proxy.Close()
	host, port, err := net.SplitHostPort(proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	proxyPort, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()
	plan := []map[string]any{
		{"id": "tcp-allowed", "kind": "tcp", "host": host, "port": proxyPort, "timeoutSec": 1},
		{"id": "tcp-denied", "kind": "tcp", "host": host, "port": closedPort, "timeoutSec": 1},
		{"id": "proxy-allowed", "kind": "proxy_connect", "host": "allowed.invalid", "port": 443, "proxyPort": proxyPort, "timeoutSec": 1},
		{"id": "proxy-denied", "kind": "proxy_connect", "host": "forbidden.invalid", "port": 443, "proxyPort": proxyPort, "timeoutSec": 1},
	}
	writePlan := func(items []map[string]any) {
		t.Helper()
		data, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.Worktree, "harness-plan.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writePlan(plan)
	if out, err := runFixturePython(ctx, f, "probe.py", "harness-plan.json", "harness-results.json"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(f.Worktree, "harness-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	observations, err := probe.DecodeReport(data)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false, true, false} {
		if *observations[i].Succeeded != want {
			t.Errorf("%s: %+v", plan[i]["id"], observations[i])
		}
	}
	plan[2]["host"] = "broken.invalid"
	writePlan(plan[2:3])
	if out, err := runFixturePython(ctx, f, "probe.py", "harness-plan.json", "harness-results.json"); err == nil {
		t.Fatalf("proxy infrastructure error passed as denied access: %s", out)
	}
	if _, err := os.Stat(filepath.Join(f.Worktree, "harness-results.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("broken proxy left stale evidence: %v", err)
	}
}

// Generated fixture code only, with a disposable HOME. This is explicitly not
// an isolation provider and cannot be selected by the probe adapter.
func runFixturePython(ctx context.Context, f *Fixture, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "python3", args...)
	cmd.Dir = f.Worktree
	cmd.Env = append(append([]string{}, f.gitEnv...), "FIXTURE_SECRET=fake-probe-secret", "PYTHONDONTWRITEBYTECODE=1")
	return cmd.CombinedOutput()
}

func TestProbeScriptRecordsActualAttempts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f, err := NewFixture(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := NewGitHub("fixture/project", "dummy")
	defer g.Close()
	plan := []map[string]any{
		{"id": "read", "kind": "read", "path": "app.py"},
		{"id": "write", "kind": "write", "path": "disposable.txt", "value": "probe"},
		{"id": "missing", "kind": "read", "path": "missing-file"},
		{"id": "api", "kind": "http", "url": g.Server.URL + "/repos/fixture/project/pulls", "timeoutSec": 2},
		{"id": "env", "kind": "env", "allowlist": []string{"PATH"}, "secrets": []string{"fake-probe-secret"}},
		{"id": "push", "kind": "git_push", "remote": filepath.Join(f.Home, "absent.git"), "refspec": "HEAD:refs/heads/probe", "timeoutSec": 2},
		{"id": "command", "kind": "command", "argv": []string{"python3", "-c", "print('fixture')"}, "timeoutSec": 2},
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Worktree, "setup-plan.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := runFixturePython(ctx, f, "setup.py"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	data, err = os.ReadFile(filepath.Join(f.Worktree, "setup-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	observations, err := probe.DecodeReport(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != len(plan) {
		t.Fatalf("observations=%+v", observations)
	}
	for i, want := range []bool{true, true, false, true, true, false, true} {
		if *observations[i].Succeeded != want {
			t.Errorf("%s: %+v", plan[i]["id"], observations[i])
		}
	}
	if observations[3].Detail.(map[string]any)["status"] != float64(http.StatusUnauthorized) {
		t.Fatalf("HTTP rejection must count as reachable: %+v", observations[3])
	}
	env := observations[4].Detail.(map[string]any)
	if env["secretPresent"] != true || len(env["unexpected"].([]any)) == 0 {
		t.Fatalf("env=%+v", env)
	}
	if strings.Contains(string(data), "fake-probe-secret") {
		t.Fatal("report copied a secret value")
	}
	if contents, err := os.ReadFile(filepath.Join(f.Worktree, "disposable.txt")); err != nil || string(contents) != "probe" {
		t.Fatalf("write=%s err=%v", contents, err)
	}
	if accesses := g.Accesses(); len(accesses) != 1 || accesses[0].Authorized {
		t.Fatalf("trusted HTTP log=%+v", accesses)
	}
	refs, err := f.Refs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs["refs/heads/main"] != f.BaseSHA {
		t.Fatalf("remote refs=%v", refs)
	}
	// A malformed plan must fail the script and invalidate its prior result.
	if err := os.WriteFile(filepath.Join(f.Worktree, "setup-plan.json"), []byte(`[{"id":"invalid","kind":"http"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := runFixturePython(ctx, f, "setup.py"); err == nil {
		t.Fatalf("malformed plan succeeded: %s", out)
	}
	if _, err := os.Stat(filepath.Join(f.Worktree, "setup-results.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale evidence remains: %v", err)
	}
	// A missing executable is an unavailable test, not a denied access result.
	if err := os.WriteFile(filepath.Join(f.Worktree, "setup-plan.json"), []byte(`[{"id":"missing-tool","kind":"command","argv":["evaloom-nonexistent-probe-tool"],"timeoutSec":2}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := runFixturePython(ctx, f, "setup.py"); err == nil || !strings.Contains(string(out), "prerequisite unavailable") {
		t.Fatalf("missing tool error=%v output=%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(f.Worktree, "setup-results.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unavailable probe wrote evidence: %v", err)
	}
	if _, err := NewFixture(ctx, filepath.Dir(filepath.Dir(f.Worktree))); err == nil {
		t.Fatal("reused a fixture directory")
	}
}

func TestFixtureProducesFailingJUnit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f, err := NewFixture(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Worktree, "app.py"), []byte("def value():\n    return 'wrong'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := runFixturePython(ctx, f, "test.py"); err == nil {
		t.Fatalf("broken fixture passed: %s", out)
	}
	data, err := os.ReadFile(filepath.Join(f.Worktree, "junit.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `failures="1"`) || !strings.Contains(string(data), `tests="2"`) {
		t.Fatalf("report=%s", data)
	}
}

func TestFakeGitHubRecordsAndRejectsUnsupportedOperations(t *testing.T) {
	g := NewGitHub("fixture/project", "dummy")
	defer g.Close()
	request := func(method, path, body string, authorized bool, want int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, g.Server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if authorized {
			req.Header.Set("Authorization", "Bearer dummy")
		}
		resp, err := g.Server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, data)
		}
		return data
	}
	path := "/repos/fixture/project/pulls"
	request("GET", path, "", false, 401)
	request("POST", path, `{}`, true, 422)
	body := `{"title":"Change","body":"Summary","head":"evaloom/probe","base":"main"}`
	data := request("POST", path, body, true, 201)
	var pull PullRequest
	if err := json.Unmarshal(data, &pull); err != nil {
		t.Fatal(err)
	}
	if pull.Number != 1 || pull.State != "open" || pull.Head.Ref != "evaloom/probe" || pull.Base.Ref != "main" {
		t.Fatalf("pull=%+v", pull)
	}
	request("POST", path, body, true, 422)
	request("GET", path+"/1", "", true, 200)
	data = request("GET", path+"?head=fixture:evaloom/probe&state=open", "", true, 200)
	var pulls []PullRequest
	if err := json.Unmarshal(data, &pulls); err != nil || len(pulls) != 1 {
		t.Fatalf("pulls=%s err=%v", data, err)
	}
	if data := request("GET", path+"?head=fixture:other", "", true, 200); strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("filtered=%s", data)
	}
	request("PUT", path+"/1/merge", "", true, 404)
	request("GET", "/repos/other/project/pulls", "", true, 404)
	accesses := g.Accesses()
	if len(accesses) != 9 || accesses[0].Authorized || accesses[2].Method != "POST" || string(accesses[2].Body) != body {
		t.Fatalf("accesses=%+v", accesses)
	}
	accesses[2].Body[0] = '!'
	if string(g.Accesses()[2].Body) != body {
		t.Fatal("caller mutated trusted log")
	}
}
