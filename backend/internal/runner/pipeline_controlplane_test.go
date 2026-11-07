package runner

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/dummy"
	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// This executable replaces only the container daemon/CLI. Product Container
// still validates mounts, creates argv, supervises the process and reads status.
func TestProductionPipelineRuntimeHelper(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 {
		return
	}
	root, args := os.Args[i+1], os.Args[i+2:]
	if args[0] == "image" {
		fmt.Print(`{"Os":"linux","Config":{}}`)
		os.Exit(0)
	}
	name := args[len(args)-1]
	if i := slices.Index(args, "--name"); i >= 0 {
		name = args[i+1]
	}
	path := filepath.Join(root, name)
	switch args[1] {
	case "create":
		data, _ := json.Marshal(args)
		if os.WriteFile(path, data, 0600) != nil {
			os.Exit(80)
		}
		fmt.Print(name)
	case "start":
		data, err := os.ReadFile(path)
		if err != nil {
			os.Exit(81)
		}
		var create []string
		if json.Unmarshal(data, &create) != nil {
			os.Exit(82)
		}
		index := slices.Index(create, "-i")
		command := exec.Command("/usr/bin/env", create[index:]...)
		command.Dir = create[slices.Index(create, "--workdir")+1]
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		code := 0
		if err := command.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else {
				code = 127
			}
		}
		if os.WriteFile(path+".exit", []byte(strconv.Itoa(code)), 0600) != nil {
			os.Exit(83)
		}
	case "inspect":
		data, err := os.ReadFile(path + ".exit")
		if err != nil {
			os.Exit(84)
		}
		code, _ := strconv.Atoi(string(data))
		fmt.Printf(`{"Status":"exited","Running":false,"ExitCode":%d}`, code)
	case "rm", "kill":
	default:
		os.Exit(85)
	}
	os.Exit(0)
}

type pipelineAPIRoute struct{ endpoint string }

func (r pipelineAPIRoute) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "api.github.com" {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = "http", strings.TrimPrefix(r.endpoint, "http://")
		return http.DefaultTransport.RoundTrip(copy)
	}
	return http.DefaultTransport.RoundTrip(req)
}

type productionPipelineFixture struct {
	t                           *testing.T
	db                          *sql.DB
	config                      Config
	process                     *Runner
	adapters                    *adapter.Registry
	remote, source, runtimeRoot string
	harnessCalls                int
	mu                          sync.Mutex
	prCalls                     int
	prBodies                    []string
	tokenPermissions            []map[string]string
	server                      *httptest.Server
	cookie                      *http.Cookie
}

func newProductionPipelineFixture(t *testing.T, scenario string) *productionPipelineFixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	f := &productionPipelineFixture{t: t, runtimeRoot: filepath.Join(root, "runtime-state"), adapters: &adapter.Registry{}}
	for _, path := range []string{home, f.runtimeRoot} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.source, f.remote = filepath.Join(root, "seed"), filepath.Join(root, "remote.git")
	gate2Git(t, root, "init", "--initial-branch=main", f.source)
	gate2Write(t, filepath.Join(f.source, ".gitignore"), "report.xml\ncopied.txt\nsetup-ran\n")
	gate2Write(t, filepath.Join(f.source, "AGENTS.md"), "fixture project instructions\n")
	gate2Write(t, filepath.Join(f.source, "code"), "initial\n")
	gate2Git(t, f.source, "add", ".")
	gate2Git(t, f.source, "commit", "-m", "initial")
	profileSHA := gate2Git(t, f.source, "rev-parse", "HEAD")
	gate2Git(t, root, "init", "--bare", "--initial-branch=main", f.remote)
	gate2Git(t, f.remote, "fetch", f.source, "HEAD:refs/heads/main")
	gate2Git(t, f.remote, "config", "http.receivepack", "true")
	gate2Git(t, f.source, "remote", "add", "origin", readURL)
	gate2Write(t, filepath.Join(f.source, "copied.txt"), "copy manifest content\n")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitHTTP, _ := readGitServer(t, root, realGit, "success")
	ca := filepath.Join(root, "ca.pem")
	gate2Write(t, ca, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gitHTTP.Certificate().Raw})))
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	readGitScript(t, filepath.Join(bin, "git"), "TestReadGitHelper", realGit, gitHTTP.URL, ca, filepath.Join(root, "git-calls"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	runtime := filepath.Join(root, "runtime")
	readGitScript(t, runtime, "TestProductionPipelineRuntimeHelper", f.runtimeRoot)
	f.db, err = store.Open(context.Background(), filepath.Join(root, "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	files, err := store.NewArtifactStore(f.db, filepath.Join(root, "control-data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewUnstartedServer(nil)
	auth, err := api.NewOwnerAuth(f.db, logger, server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.adapters.Register("dummy", pipelineHarness{Adapter: dummy.New(), start: func(ctx context.Context, req adapter.HarnessRequest, env adapter.SandboxEnv) error {
		f.harnessCalls++
		deadline, ok := ctx.Deadline()
		expected := time.Duration(f.config.DefaultTimeouts.HarnessSec) * time.Second
		if scenario == "project_timeout" {
			expected = time.Second
		}
		if !ok || time.Until(deadline) > expected || time.Until(deadline) < expected-time.Second {
			t.Error("wrong harness timeout")
		}
		if scenario == "project_timeout" || scenario == "default_timeout" {
			<-ctx.Done()
			return ctx.Err()
		}
		gate2Write(t, filepath.Join(req.Project.Workdir, "code"), "dummy committed change\n")
		gate2Git(t, req.Project.Workdir, "add", "code")
		gate2Git(t, req.Project.Workdir, "commit", "-m", "dummy change")
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	handler := api.NewHandlerWithArtifacts(f.db, logger, auth, nil, api.ContextConfig{}, api.ArtifactConfig{Store: files, MaxBytes: 1 << 20}, f.adapters)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, r)
		if result.Code >= 400 {
			t.Logf("Control Plane %s: %d %s", r.URL.Path, result.Code, result.Body.String())
		}
		for key, values := range result.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(result.Code)
		_, _ = w.Write(result.Body.Bytes())
	})
	server.Start()
	f.server = server
	login, err := server.Client().Get(auth.LoginURL())
	if err != nil {
		t.Fatal(err)
	}
	if len(login.Cookies()) != 1 {
		t.Fatal("owner login")
	}
	f.cookie = login.Cookies()[0]
	login.Body.Close()
	t.Cleanup(server.Close)
	f.config = testConfig(t, server.URL)
	f.config.Container.RuntimePath = runtime
	f.config.HarnessAuthFiles = map[string]string{"h": filepath.Join(f.config.DataDir, "auth.json")}
	f.config.DefaultTimeouts = DefaultTimeouts{SetupSec: 30, HarnessSec: 30, TestSec: 30}
	if scenario == "default_timeout" {
		f.config.DefaultTimeouts.HarnessSec = 1
	}
	if scenario == "missing_auth" {
		f.config.HarnessAuthFiles = map[string]string{}
	}
	token, err := runnerauth.Register(context.Background(), f.db, f.config.RunnerID, "Fixture", f.config.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config.TokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.config.GitHubAppPrivateKeyFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config.GitHubAppPrivateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	setup := "printf setup > setup-ran"
	if scenario == "setup_failure" {
		setup = "exit 7"
	}
	command := `printf '%s' '<testsuite><testcase name="existing"/></testsuite>' > report.xml`
	if scenario == "gate_failure" {
		command = `if grep -q dummy code; then exit 9; fi; ` + command
	}
	if scenario == "resume" {
		command = `if grep -q dummy code; then printf '%s' '<testsuite><testcase name="existing"/></testsuite>' > report.xml; else printf '%s' '<testsuite><testcase name="existing"/><testcase name="removed"/></testsuite>' > report.xml; fi`
	}
	timeouts := "{}"
	if scenario == "project_timeout" {
		timeouts = `{"harnessSec":1}`
	}
	definition := `{"profileVersion":"1.0.0","harnessVersion":"fixture-1","modelId":"fixture","contextFiles":[],"executionConfiguration":{"adapter":"dummy","args":[]}}`
	hash := sha256.Sum256([]byte(definition))
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO projects(id,name,local_repo_path,git_remote_url,runner_id,base_branch,default_role,context_location,copy_allowlist,allowed_operations,approval_policy,timeouts,setup_command,test_command,test_report_format,test_report_path,created_at,updated_at) VALUES('project','Project',?,?,'mac-mini','main','developer','AGENTS.md','["copied.txt","absent.txt"]','{}','{}',?,?,?,'junit_xml','report.xml','now','now')`, []any{f.source, readURL, timeouts, setup, command}},
		{`INSERT INTO tasks(id,project_id,title,body,acceptance_criteria,short_name,task_type,collaboration_mode,status,idempotency_key,created_at,updated_at) VALUES('task-001','project','Pipeline','body','verified push','pipeline','development','solo','queued','pipeline','now','now')`, nil},
		{`INSERT INTO harnesses(id,name,specialty,supported_tasks,adapter,repo_path,repo_url,supported_models,available_tools,tool_permissions,contract_version,created_at,updated_at) VALUES('h','Harness','developer','["development"]','dummy',?,'','[]','[]','{}','1','now','now')`, []any{f.source}},
		{`INSERT INTO harness_profiles(id,harness_id,version,commit_sha,tag_name,definition,definition_hash,model_id,operation_status,registered_at) VALUES('p','h','1.0.0',?,'v1',?,?,'fixture','provisional','now')`, []any{profileSHA, definition, hex.EncodeToString(hash[:])}},
		{`INSERT INTO project_harnesses VALUES('project','developer','h','p','now')`, nil},
		{`INSERT INTO executions(id,task_id,seq,mode,status,created_at) VALUES(?,'task-001',1,'fresh','pending','now')`, []any{logExecution}},
	} {
		if _, err := f.db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	github := httptest.NewServer(http.HandlerFunc(f.github))
	t.Cleanup(github.Close)
	f.process, err = New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	f.process.client.Transport = pipelineAPIRoute{github.URL}
	f.process.pollInterval = 20 * time.Millisecond
	if scenario == "path_escape" {
		if err := os.MkdirAll(f.config.WorktreeRoot, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(root, filepath.Join(f.config.WorktreeRoot, "task-001-r1")); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *productionPipelineFixture) github(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/access_tokens") {
		var input struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || !slices.Equal(input.Repositories, []string{"repo"}) {
			f.t.Error("invalid token scope")
		}
		f.tokenPermissions = append(f.tokenPermissions, input.Permissions)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": readCanary, "expires_at": time.Now().Add(time.Hour), "permissions": input.Permissions, "repositories": []map[string]string{{"full_name": "owner/repo"}}})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+readCanary {
		f.t.Error("missing GitHub authentication")
	}
	if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/7") {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "html_url": "https://github.com/owner/repo/pull/7", "state": "open", "merged": false, "head": map[string]string{"sha": strings.Repeat("a", 40)}})
		return
	}
	if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls") {
		io.WriteString(w, "[]")
		return
	}
	if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls") {
		var input map[string]string
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			f.t.Error("invalid PR body")
		}
		var pushed, verified string
		if err := f.db.QueryRow(`SELECT coalesce(pushed_sha,''),coalesce(verified_sha,'') FROM executions WHERE id=?`, logExecution).Scan(&pushed, &verified); err != nil || pushed == "" || pushed != verified {
			f.t.Errorf("PR before durable push: %s/%s %v", pushed, verified, err)
		}
		f.prCalls++
		f.prBodies = append(f.prBodies, input["body"])
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "html_url": "https://github.com/owner/repo/pull/1", "state": "open", "head": map[string]any{"ref": input["head"], "repo": map[string]string{"full_name": "owner/repo"}}, "base": map[string]string{"ref": input["base"]}})
		return
	}
	f.t.Errorf("unexpected GitHub %s %s", r.Method, r.URL.Path)
	w.WriteHeader(404)
}

func (f *productionPipelineFixture) execute(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var executionErr error
	err := f.process.Run(ctx, func(ctx context.Context, c Claim) {
		deps := f.process.PipelineDependenciesForClaim(f.config, c)
		deps.Adapters = f.adapters // The only non-runtime product replacement is dummy.
		executionErr = f.process.ExecuteClaim(ctx, context.Background(), f.config, c, deps)
		cancel()
	}, func(err error) { t.Error(err) })
	if err != nil {
		t.Fatal(err)
	}
	return executionErr
}

func TestProductionPipelineClaimPushAndSQLite(t *testing.T) {
	f := newProductionPipelineFixture(t, "success")
	if err := f.execute(t); err != nil {
		var stage *StageError
		if errors.As(err, &stage) {
			t.Log(stage.Err)
		}
		t.Fatal(err)
	}
	f.assertSucceeded(t)
}
func (f *productionPipelineFixture) assertSucceeded(t *testing.T) {
	t.Helper()
	var status, base, branch, worktreeID, session, verified, pushed, prURL string
	var pr int
	if err := f.db.QueryRow(`SELECT status,base_sha,branch_name,worktree_id,harness_session_id,verified_sha,pushed_sha,pr_number,pr_url FROM executions WHERE id=?`, logExecution).Scan(&status, &base, &branch, &worktreeID, &session, &verified, &pushed, &pr, &prURL); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || base == "" || worktreeID == "" || session == "" || verified == "" || pushed != verified || pr != 1 || prURL != "https://github.com/owner/repo/pull/1" || !strings.HasSuffix(branch, "-r1") {
		t.Fatalf("execution %s %s %s %s %s %s %s %d %s", status, base, branch, worktreeID, session, verified, pushed, pr, prURL)
	}
	if got := gate2Git(t, f.remote, "rev-parse", "refs/heads/"+branch); got != verified {
		t.Fatalf("remote %s != verified %s", got, verified)
	}
	var manifest string
	if err := f.db.QueryRow(`SELECT copied_files FROM worktrees WHERE id=?`, worktreeID).Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	var copies []struct{ Path, SHA256 string }
	if json.Unmarshal([]byte(manifest), &copies) != nil || len(copies) != 1 || copies[0].Path != "copied.txt" {
		t.Fatalf("manifest %s", manifest)
	}
	digest := sha256.Sum256([]byte("copy manifest content\n"))
	if copies[0].SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("copy digest %s", manifest)
	}
	for _, kind := range []string{"worktree_ready", "pushed", "pr_created"} {
		var count int
		if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE execution_id=? AND type=?`, logExecution, kind).Scan(&count); err != nil || count != 1 {
			t.Fatalf("event %s count %d %v", kind, count, err)
		}
	}
	var wall int64
	var input, output sql.NullInt64
	if err := f.db.QueryRow(`SELECT wall_time_ms,input_tokens,output_tokens FROM usage_records WHERE execution_id=?`, logExecution).Scan(&wall, &input, &output); err != nil || wall <= 0 || input.Valid || output.Valid {
		t.Fatalf("usage %d %v %v %v", wall, input, output, err)
	}
	if f.harnessCalls != 1 || f.prCalls != 1 {
		t.Fatalf("harness/PR %d/%d", f.harnessCalls, f.prCalls)
	}
	read := map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"}
	write := map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write"}
	if !reflect.DeepEqual(f.tokenPermissions, []map[string]string{read, read, write, write, write}) {
		t.Fatalf("stage-scoped token permissions: %v", f.tokenPermissions)
	}
	if !strings.Contains(f.prBodies[0], "Gate 3: pass") {
		t.Fatal("PR lacks Gate evidence")
	}
	entries, err := os.ReadDir(f.runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	creates := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".exit") {
			continue
		}
		creates++
		data, err := os.ReadFile(filepath.Join(f.runtimeRoot, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var args []string
		if json.Unmarshal(data, &args) != nil {
			t.Fatal("bad runtime invocation")
		}
		if args[slices.Index(args, "--cpus")+1] != "4" || args[slices.Index(args, "--memory")+1] != "8589934592" {
			t.Fatalf("resource limits %v", args)
		}
	}
	if creates != 3 {
		t.Fatalf("runtime creates %d", creates)
	}
}

func TestProductionPipelineStopsAndRetainsWorktree(t *testing.T) {
	for _, scenario := range []string{"setup_failure", "gate_failure", "missing_auth", "path_escape", "project_timeout", "default_timeout"} {
		t.Run(scenario, func(t *testing.T) {
			f := newProductionPipelineFixture(t, scenario)
			if err := f.execute(t); err == nil {
				t.Fatal("expected stop")
			}
			var status, class string
			if err := f.db.QueryRow(`SELECT status,error_class FROM executions WHERE id=?`, logExecution).Scan(&status, &class); err != nil {
				t.Fatal(err)
			}
			wantClass := "config_error"
			if scenario == "gate_failure" {
				wantClass = "test_failed"
			}
			if strings.HasSuffix(scenario, "timeout") {
				wantClass = "timeout"
			}
			wantStatus := "failed"
			if wantClass == "timeout" {
				wantStatus = "timed_out"
			}
			if status != wantStatus || class != wantClass || f.prCalls != 0 {
				t.Fatalf("status=%s class=%s PR=%d", status, class, f.prCalls)
			}
			wantHarness := 1
			if scenario == "missing_auth" || scenario == "path_escape" || scenario == "setup_failure" {
				wantHarness = 0
			}
			if f.harnessCalls != wantHarness {
				t.Fatalf("later harness ran %d", f.harnessCalls)
			}
			if scenario == "path_escape" {
				var detail string
				if err := f.db.QueryRow(`SELECT detail FROM audit_logs WHERE action='worktree.path_rejected'`).Scan(&detail); err != nil || !strings.Contains(detail, filepath.Dir(f.source)) {
					t.Fatalf("audit %s %v", detail, err)
				}
			}
			if scenario != "missing_auth" && scenario != "path_escape" {
				var path string
				if err := f.db.QueryRow(`SELECT path FROM worktrees`).Scan(&path); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("worktree not retained", err)
				}
			}
			var pushes int
			if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE type='pushed'`).Scan(&pushes); err != nil || pushes != 0 {
				t.Fatal("continued to push", err)
			}
		})
	}
}

func TestProductionPipelineFreshOpenPRBlocksBeforeWorktree(t *testing.T) {
	f := newProductionPipelineFixture(t, "success")
	if _, err := f.db.Exec(`UPDATE executions SET seq=2; INSERT INTO executions(id,task_id,seq,mode,status,branch_name,pr_number,pr_url,created_at) VALUES('01ARZ3NDEKTSV4RRFFQ69G5FAW','task-001',1,'continue','succeeded','evaloom/task-001-pipeline',7,'https://github.com/owner/repo/pull/7','before')`); err != nil {
		t.Fatal(err)
	}
	if err := f.execute(t); err == nil {
		t.Fatal("expected PR Block")
	}
	var status, cause, head string
	if err := f.db.QueryRow(`SELECT e.status,b.cause,b.head_sha FROM executions e JOIN execution_blocks b ON b.execution_id=e.id WHERE e.id=?`, logExecution).Scan(&status, &cause, &head); err != nil || status != "blocked" || cause != "git_state" || head != strings.Repeat("a", 40) {
		t.Fatalf("%s %s %s %v", status, cause, head, err)
	}
	if _, err := os.Stat(f.config.WorktreeRoot); !os.IsNotExist(err) {
		t.Fatal("created worktree before PR check", err)
	}
	if f.harnessCalls != 0 || f.prCalls != 0 {
		t.Fatal("continued after PR Block")
	}
}

func TestProductionPipelineResumeGate3UsesSavedBlock(t *testing.T) {
	f := newProductionPipelineFixture(t, "resume")
	if err := f.execute(t); err == nil {
		t.Fatal("expected Gate 2 Block")
	}
	var cause string
	if err := f.db.QueryRow(`SELECT cause FROM execution_blocks WHERE execution_id=?`, logExecution).Scan(&cause); err != nil {
		t.Fatal(err)
	}
	if f.harnessCalls != 1 || f.prCalls != 0 {
		t.Fatal("initial stages did not stop")
	}
	owner := &baselineHTTPFixture{logControlPlane: &logControlPlane{db: f.db, server: f.server, cookie: f.cookie}}
	owner.continueBlocked(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	claim, err := f.process.claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("resume claim %v %v", claim, err)
	}
	if claim.Execution.ResumePoint == nil || *claim.Execution.ResumePoint != "gate3" || claim.Execution.ResumeBlock == nil || claim.Execution.ResumeHarnessSummary == nil || !strings.Contains(*claim.Execution.ResumeHarnessSummary, "dummy completed") || len(claim.Execution.ResumeGateResults) != 1 {
		t.Fatalf("resume input %+v", claim.Execution)
	}
	deps := f.process.PipelineDependenciesForClaim(f.config, *claim)
	deps.Adapters = f.adapters
	if err := f.process.ExecuteClaim(ctx, ctx, f.config, *claim, deps); err != nil {
		var stage *StageError
		if errors.As(err, &stage) {
			t.Log(stage.Err)
		}
		t.Fatal(err)
	}
	f.assertSucceeded(t)
	if !strings.Contains(f.prBodies[0], "本人が続行を選んだ Block") || !strings.Contains(f.prBodies[0], cause) || !strings.Contains(f.prBodies[0], claim.Execution.ResumeBlock.HeadSHA) {
		t.Fatalf("approved Block missing from PR: %s", f.prBodies[0])
	}
	var tests int
	if err := f.db.QueryRow(`SELECT count(*) FROM test_runs WHERE execution_id=?`, logExecution).Scan(&tests); err != nil || tests != 3 {
		t.Fatalf("resume reran setup/tests: %d %v", tests, err)
	}
}

func TestProductionPipelineResumeRejectsChangedWorktree(t *testing.T) {
	for _, scenario := range []string{"path_escape", "changed_head", "git_pointer_escape"} {
		t.Run(scenario, func(t *testing.T) {
			f := newProductionPipelineFixture(t, "resume")
			if err := f.execute(t); err == nil {
				t.Fatal("expected Gate 2 Block")
			}
			owner := &baselineHTTPFixture{logControlPlane: &logControlPlane{db: f.db, server: f.server, cookie: f.cookie}}
			owner.continueBlocked(t)
			var path string
			if err := f.db.QueryRow(`SELECT path FROM worktrees`).Scan(&path); err != nil {
				t.Fatal(err)
			}
			if scenario == "path_escape" {
				if _, err := f.db.Exec(`UPDATE worktrees SET path=?`, f.source); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			claim, err := f.process.claim(ctx)
			if err != nil || claim == nil || claim.Worktree == nil || claim.Execution.ResumePoint == nil || *claim.Execution.ResumePoint != "gate3" {
				t.Fatalf("resume claim: %v %v", claim, err)
			}
			head := claim.Execution.ResumeBlock.HeadSHA
			switch scenario {
			case "path_escape":
				// The saved path itself must be checked before any Git operation.
				if claim.Worktree.Path != f.source {
					t.Fatal("claim did not return the saved path")
				}
			case "git_pointer_escape":
				// The apparent task path is inside the root but its Git area is not.
				gate2Write(t, filepath.Join(path, ".git"), "gitdir: "+filepath.Join(f.source, ".git")+"\n")
			case "changed_head":
				gate2Write(t, filepath.Join(path, "code"), "changed after owner decision\n")
				gate2Git(t, path, "commit", "-am", "advance after decision")
				head = gate2Git(t, path, "rev-parse", "HEAD")
			}
			deps := f.process.PipelineDependenciesForClaim(f.config, *claim)
			deps.Adapters = f.adapters
			var stage *StageError
			if err := f.process.ExecuteClaim(ctx, ctx, f.config, *claim, deps); !errors.As(err, &stage) || stage.Stage.ID != StageGate3 {
				t.Fatalf("resume must stop at Gate 3: %v", err)
			}
			var status string
			var class sql.NullString
			if err := f.db.QueryRow(`SELECT status,error_class FROM executions WHERE id=?`, logExecution).Scan(&status, &class); err != nil {
				t.Fatal(err)
			}
			if scenario == "changed_head" {
				var observed string
				if err := f.db.QueryRow(`SELECT head_sha FROM execution_blocks WHERE execution_id=? AND decision IS NULL`, logExecution).Scan(&observed); err != nil || status != "blocked" || observed != head {
					t.Fatalf("changed HEAD: %s %s %v", status, observed, err)
				}
			} else {
				var detail string
				if err := f.db.QueryRow(`SELECT detail FROM audit_logs WHERE action='worktree.path_rejected'`).Scan(&detail); err != nil || status != "failed" || class.String != "config_error" || !strings.Contains(detail, claim.Worktree.Path) {
					t.Fatalf("path refusal: %s %s %s %v", status, class.String, detail, err)
				}
			}
			if f.harnessCalls != 1 || f.prCalls != 0 {
				t.Fatal("resume reran the harness or published")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("worktree was not retained", err)
			}
		})
	}
}
