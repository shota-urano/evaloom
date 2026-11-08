package runner

import (
	"bytes"
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
	"github.com/shota-urano/evaloom/backend/internal/linearoutbox"
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
	harnessOutputs              []harnessOutputAttempt
	runnerToken, secretSummary  string
	// publishChecks counts POST /api/executions/{id}/publish-check calls.
	publishChecks, prSearches int
	initialMain               string
	// advancedMain is the remote Base Branch moved by another push (3.7.3).
	advancedMain string
}

// secretSummaryHarness reports a summary chosen by the test, as a harness
// that wrote secrets and paths into its summary would.
type secretSummaryHarness struct {
	adapter.Adapter
	summary func() string
}

func (h *secretSummaryHarness) Result(id adapter.SessionID) (adapter.HarnessResult, error) {
	result, err := h.Adapter.Result(id)
	result.Summary = h.summary()
	return result, err
}

type harnessOutputAttempt struct {
	id   string
	body []byte
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
	f.initialMain = gate2Git(t, f.remote, "rev-parse", "refs/heads/main")
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
	// As evaloomd: Linear comment bodies read 作業概要 from harness_output.
	t.Cleanup(linearoutbox.SetSummarySource(files.HarnessSummaryTx))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewUnstartedServer(nil)
	auth, err := api.NewOwnerAuth(f.db, logger, server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var harness adapter.Adapter = dummy.New()
	if scenario == "linear_feedback" {
		harness = &secretSummaryHarness{Adapter: harness, summary: func() string { return f.secretSummary }}
	}
	if err := f.adapters.Register("dummy", pipelineHarness{Adapter: harness, start: func(ctx context.Context, req adapter.HarnessRequest, env adapter.SandboxEnv) error {
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
		if scenario == "linear_feedback" {
			f.secretSummary = strings.Join([]string{"作業した", f.runnerToken, readCanary, req.Project.Workdir}, " ")
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
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/publish-check") {
			f.mu.Lock()
			f.publishChecks++
			f.mu.Unlock()
			switch scenario {
			case "direct_push_policy_changed":
				// The owner switches back to require_approval right before the push.
				if _, err := f.db.Exec(`UPDATE projects SET approval_policy='{"mergePullRequest":"require_approval","version":1}' WHERE id='project'`); err != nil {
					t.Error(err)
				}
			case "direct_push_not_fast_forward":
				// Someone else pushes to the Base Branch after the task started.
				f.advancedMain = gate2Git(t, f.remote, "commit-tree", "-p", f.initialMain, "-m", "pushed by someone else", f.initialMain+"^{tree}")
				gate2Git(t, f.remote, "update-ref", "refs/heads/main", f.advancedMain, f.initialMain)
			case "direct_push_cancelled":
				// The owner cancels the task right before the push.
				if _, err := f.db.Exec(`UPDATE executions SET cancel_requested_at='2026-11-08T00:00:00Z' WHERE id=?`, logExecution); err != nil {
					t.Error(err)
				}
			case "direct_push_unreachable":
				// The Control Plane cannot be reached: drop the connection unanswered.
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				connection.Close()
				return
			}
		}
		failOutput := false
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/artifacts") && r.URL.Query().Get("kind") == "harness_output" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			id := r.URL.Query().Get("artifactId")
			local, err := os.ReadFile(filepath.Join(f.config.DataDir, "artifacts", id+".json"))
			if err != nil || !bytes.Equal(local, body) {
				t.Errorf("harness output not retained locally: %v", err)
			}
			f.mu.Lock()
			f.harnessOutputs = append(f.harnessOutputs, harnessOutputAttempt{id: id, body: body})
			failOutput = len(f.harnessOutputs) == 1
			f.mu.Unlock()
			if failOutput && scenario == "output_unavailable" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, r)
		if failOutput && scenario == "output_lost_ack" {
			if result.Code < 200 || result.Code >= 300 {
				t.Errorf("lost acknowledgement requires a committed artifact: %d %s", result.Code, result.Body.String())
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
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
	f.runnerToken = token
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
	// Spec 17 3.3: by default a published PR waits for approval. The Linear
	// body scenario checks the succeeded comment, so it excludes PR merges
	// from approval and completes on the PR as in Phase 1.
	policy := `{"mergePullRequest":"require_approval","version":1}`
	if scenario == "linear_feedback" || strings.HasPrefix(scenario, "direct_push") {
		policy = `{"mergePullRequest":"direct_push","version":1}`
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
		{`INSERT INTO projects(id,name,local_repo_path,git_remote_url,runner_id,base_branch,default_role,context_location,copy_allowlist,allowed_operations,approval_policy,timeouts,setup_command,test_command,test_report_format,test_report_path,created_at,updated_at) VALUES('project','Project',?,?,'mac-mini','main','developer','AGENTS.md','["copied.txt","absent.txt"]','{}',?,?,?,?,'junit_xml','report.xml','now','now')`, []any{f.source, readURL, policy, timeouts, setup, command}},
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
	if scenario == "linear_feedback" {
		for _, statement := range []string{
			`UPDATE tasks SET source='linear' WHERE id='task-001'`,
			`INSERT INTO linear_issue_links(id,linear_issue_id,issue_identifier,issue_url,team_id,task_id,attach_seq,created_at) VALUES('link-1','issue-1','ENG-1','https://linear.app/x/ENG-1','team-a','task-001',1,'now')`,
		} {
			if _, err := f.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
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
		f.prSearches++
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

func TestProductionPipelineRetriesHarnessOutput(t *testing.T) {
	for _, scenario := range []string{"output_unavailable", "output_lost_ack"} {
		t.Run(scenario, func(t *testing.T) {
			f := newProductionPipelineFixture(t, scenario)
			if err := f.execute(t); err != nil {
				t.Fatal(err)
			}
			f.assertSucceeded(t)
			f.mu.Lock()
			attempts := append([]harnessOutputAttempt(nil), f.harnessOutputs...)
			f.mu.Unlock()
			if len(attempts) != 2 || attempts[0].id == "" || attempts[0].id != attempts[1].id || !bytes.Equal(attempts[0].body, attempts[1].body) {
				t.Fatalf("harness output retry changed identity/content: %+v", attempts)
			}
			var result adapter.HarnessResult
			if err := json.Unmarshal(attempts[0].body, &result); err != nil || result.Summary != "dummy completed "+logExecution || result.HeadSHA == "" || result.DiffArtifactID == "" {
				t.Fatalf("harness output lost result: %+v %v", result, err)
			}
			var count int
			if err := f.db.QueryRow(`SELECT count(*) FROM artifacts WHERE execution_id=? AND kind='harness_output'`, logExecution).Scan(&count); err != nil || count != 1 {
				t.Fatalf("harness output rows=%d err=%v", count, err)
			}
			var hash string
			var size int64
			if err := f.db.QueryRow(`SELECT sha256,size_bytes FROM artifacts WHERE id=? AND execution_id=? AND kind='harness_output'`, attempts[0].id, logExecution).Scan(&hash, &size); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(attempts[0].body)
			if hash != hex.EncodeToString(digest[:]) || size != int64(len(attempts[0].body)) {
				t.Fatalf("stored harness output differs: hash=%s size=%d", hash, size)
			}
			path := filepath.Join(f.config.DataDir, "artifacts", attempts[0].id+".json")
			if local, err := os.ReadFile(path); err != nil || !bytes.Equal(local, attempts[0].body) {
				t.Fatalf("saved harness output differs after retry: %v", err)
			}
		})
	}
}

func (f *productionPipelineFixture) assertSucceeded(t *testing.T) {
	t.Helper()
	var status, base, branch, worktreeID, session, verified, pushed string
	var pr sql.NullInt64
	var prURL sql.NullString
	if err := f.db.QueryRow(`SELECT status,base_sha,branch_name,worktree_id,harness_session_id,verified_sha,pushed_sha,pr_number,pr_url FROM executions WHERE id=?`, logExecution).Scan(&status, &base, &branch, &worktreeID, &session, &verified, &pushed, &pr, &prURL); err != nil {
		t.Fatal(err)
	}
	var policy, taskStatus string
	if err := f.db.QueryRow(`SELECT json_extract(p.approval_policy,'$.mergePullRequest'),t.status FROM projects p JOIN tasks t ON t.project_id=p.id WHERE t.id='task-001'`).Scan(&policy, &taskStatus); err != nil {
		t.Fatal(err)
	}
	// Spec 17 3.3: with require_approval the published PR waits for approval.
	// Spec 17 3.7: with direct_push the verified commit goes to the Base
	// Branch and no PR exists.
	direct := policy == "direct_push"
	want, wantTask := "waiting_approval", "waiting_approval"
	if direct {
		want, wantTask = "succeeded", "completed"
	}
	if status != want || taskStatus != wantTask || base == "" || worktreeID == "" || session == "" || verified == "" || pushed != verified || !strings.HasSuffix(branch, "-r1") {
		t.Fatalf("execution %s/%s %s %s %s %s %s %s", status, taskStatus, base, branch, worktreeID, session, verified, pushed)
	}
	if direct {
		if pr.Valid || prURL.Valid {
			t.Fatalf("direct push recorded a PR: %v %v", pr, prURL)
		}
	} else if pr.Int64 != 1 || prURL.String != "https://github.com/owner/repo/pull/1" {
		t.Fatalf("PR %v %v", pr, prURL)
	}
	var approvals int
	if err := f.db.QueryRow(`SELECT count(*) FROM approvals WHERE execution_id=? AND status='pending' AND content_version=?
		AND json_extract(target,'$.repository')='owner/repo' AND json_extract(target,'$.prNumber')=1 AND json_extract(target,'$.headBranch')=?`,
		logExecution, pushed, branch).Scan(&approvals); err != nil || approvals != map[bool]int{true: 1, false: 0}[want == "waiting_approval"] {
		t.Fatalf("pending approvals %d %v", approvals, err)
	}
	if direct {
		// 3.7.2 step 2: the remote Base Branch is the verified commit, and the
		// task branch is kept only in the Runner's clone.
		if got := gate2Git(t, f.remote, "rev-parse", "refs/heads/main"); got != verified {
			t.Fatalf("remote main %s != verified %s", got, verified)
		}
		if refs := gate2Git(t, f.remote, "for-each-ref", "--format=%(refname)", "refs/heads/evaloom/"); refs != "" {
			t.Fatalf("task branch pushed: %s", refs)
		}
	} else {
		if got := gate2Git(t, f.remote, "rev-parse", "refs/heads/"+branch); got != verified {
			t.Fatalf("remote %s != verified %s", got, verified)
		}
		if got := gate2Git(t, f.remote, "rev-parse", "refs/heads/main"); got != f.initialMain {
			t.Fatalf("PR path moved main to %s", got)
		}
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
	for kind, wantCount := range map[string]int{"worktree_ready": 1, "pushed": 1, "pr_created": map[bool]int{true: 0, false: 1}[direct]} {
		var count int
		if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE execution_id=? AND type=?`, logExecution, kind).Scan(&count); err != nil || count != wantCount {
			t.Fatalf("event %s count %d %v", kind, count, err)
		}
	}
	var wall int64
	var input, output sql.NullInt64
	if err := f.db.QueryRow(`SELECT wall_time_ms,input_tokens,output_tokens FROM usage_records WHERE execution_id=?`, logExecution).Scan(&wall, &input, &output); err != nil || wall <= 0 || input.Valid || output.Valid {
		t.Fatalf("usage %d %v %v %v", wall, input, output, err)
	}
	read := map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"}
	write := map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write"}
	if direct {
		// No PR search or creation request reaches GitHub; only the push token.
		if f.harnessCalls != 1 || f.prCalls != 0 || f.prSearches != 0 || f.publishChecks != 1 {
			t.Fatalf("harness/PR/search/check %d/%d/%d/%d", f.harnessCalls, f.prCalls, f.prSearches, f.publishChecks)
		}
		if !reflect.DeepEqual(f.tokenPermissions, []map[string]string{read, read, write}) {
			t.Fatalf("stage-scoped token permissions: %v", f.tokenPermissions)
		}
	} else {
		if f.harnessCalls != 1 || f.prCalls != 1 {
			t.Fatalf("harness/PR %d/%d", f.harnessCalls, f.prCalls)
		}
		wantTokens := []map[string]string{read, read, write, write, write}
		if f.publishChecks != 0 {
			// The direct attempt acquired a push token before publish-check
			// selected the PR path.
			wantTokens = []map[string]string{read, read, write, write, write, write}
		}
		if !reflect.DeepEqual(f.tokenPermissions, wantTokens) {
			t.Fatalf("stage-scoped token permissions: %v", f.tokenPermissions)
		}
		if !strings.Contains(f.prBodies[0], "Gate 3: pass") {
			t.Fatal("PR lacks Gate evidence")
		}
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
