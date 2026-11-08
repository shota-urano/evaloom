package runner

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const operationExecution = "01ARZ3NDEKTSV4RRFFQ69G5FAW"

// operationFixture is a real Control Plane (handlers, auth, SQLite) with one
// Execution merging under an approved merge approval, a Runner, and a test
// GitHub API server that records every merge request.
type operationFixture struct {
	t          *testing.T
	db         *sql.DB
	config     Config
	process    *Runner
	approvalID string
	content    string

	mu        sync.Mutex
	prHead    string // head SHA the test GitHub reports for the PR
	prOpen    bool
	mergeCode int // HTTP status of the merge answer
	merges    []map[string]string
	prMerged  bool
	prReads   int
	// dropMerge cuts the connection instead of answering the merge request
	// (spec 17 3.5.3). mergeApplies says whether GitHub merged before that.
	dropMerge    bool
	mergeApplies bool
	failReread   bool // the PR cannot be read after the first read
}

const operationMergeCommit = "1111111111111111111111111111111111111111"

func newOperationFixture(t *testing.T) *operationFixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	f := &operationFixture{t: t, content: strings.Repeat("b", 40), prOpen: true, mergeCode: http.StatusOK}
	f.prHead = f.content
	var err error
	f.db, err = store.Open(context.Background(), filepath.Join(root, "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewUnstartedServer(nil)
	auth, err := api.NewOwnerAuth(f.db, logger, server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = api.NewHandler(f.db, logger, auth)
	server.Start()
	t.Cleanup(server.Close)
	f.config = testConfig(t, server.URL)
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
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO projects(id,name,local_repo_path,git_remote_url,runner_id,base_branch,default_role,context_location,copy_allowlist,allowed_operations,approval_policy,timeouts,created_at,updated_at) VALUES('project','Project','/repo',?,'mac-mini','main','developer','AGENTS.md','[]','{}','{"mergePullRequest":"require_approval","version":1}','{}','now','now')`, []any{readURL}},
		{`INSERT INTO tasks(id,project_id,title,body,acceptance_criteria,short_name,task_type,collaboration_mode,status,idempotency_key,created_at,updated_at) VALUES('task-001','project','Merge','body','merged','merge','development','solo','waiting_approval','merge','now','now')`, nil},
		{`INSERT INTO executions(id,task_id,seq,mode,status,runner_id,branch_name,verified_sha,pushed_sha,pr_number,pr_url,created_at) VALUES(?,'task-001',1,'fresh','waiting_approval','mac-mini','evaloom/task-001-merge',?,?,1,'https://github.com/owner/repo/pull/1','now')`, []any{operationExecution, f.content, f.content}},
	} {
		if _, err := f.db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	// The approval is created exactly as after the PR was published (3.3)
	// and approved by the owner (3.5.1 step 1).
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	f.approvalID, err = store.OpenMergeApprovalTx(context.Background(), tx, operationExecution)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	f.approve(time.Now().Add(time.Hour))
	github := httptest.NewServer(http.HandlerFunc(f.github))
	t.Cleanup(github.Close)
	f.process, err = New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	f.process.client.Transport = pipelineAPIRoute{github.URL}
	f.process.pollInterval = 20 * time.Millisecond
	return f
}

func (f *operationFixture) approve(expires time.Time) {
	f.t.Helper()
	if _, err := f.db.Exec(`UPDATE approvals SET status='approved', decided_by='owner', decided_at=?, expires_at=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339Nano), expires.UTC().Format(time.RFC3339Nano), f.approvalID); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET status='merging' WHERE id=?`, operationExecution); err != nil {
		f.t.Fatal(err)
	}
}

func (f *operationFixture) github(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/access_tokens") {
		var input struct {
			Permissions map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": readCanary, "expires_at": time.Now().Add(time.Hour), "permissions": input.Permissions, "repositories": []map[string]string{{"full_name": "owner/repo"}}})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+readCanary {
		f.t.Error("missing GitHub authentication")
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
		f.prReads++
		if f.failReread && f.prReads > 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		state := "open"
		if !f.prOpen {
			state = "closed"
		}
		pr := map[string]any{"number": 1, "html_url": "https://github.com/owner/repo/pull/1", "state": state, "merged": f.prMerged,
			"head": map[string]any{"sha": f.prHead, "ref": "evaloom/task-001-merge"}, "base": map[string]string{"ref": "main"}}
		if f.prMerged {
			pr["merge_commit_sha"] = operationMergeCommit
		}
		_ = json.NewEncoder(w).Encode(pr)
	case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/pulls/1/merge":
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			f.t.Error("invalid merge body")
		}
		f.merges = append(f.merges, input)
		if f.dropMerge {
			if f.mergeApplies {
				f.prMerged, f.prOpen = true, false
			}
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				f.t.Error(err)
				return
			}
			connection.Close()
			return
		}
		w.WriteHeader(f.mergeCode)
		if f.mergeCode == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": operationMergeCommit, "merged": true, "message": "merged"})
		} else {
			io.WriteString(w, `{"message":"Head branch was modified"}`)
		}
	default:
		f.t.Errorf("unexpected GitHub %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// run polls the real claim endpoint like cmd/evaloom-runner and handles the
// first approved operation, then stops.
func (f *operationFixture) run() error {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var result error
	handled := false
	err := f.process.Run(ctx, func(ctx context.Context, c Claim) {
		defer cancel()
		handled = true
		if c.Operation == nil {
			f.t.Error("claim is not an approved operation")
			return
		}
		result = f.process.ExecuteOperation(ctx, context.Background(), f.config, c)
	}, func(err error) { f.t.Error(err) })
	if err != nil {
		f.t.Fatal(err)
	}
	if !handled {
		f.t.Fatal("no approved operation was claimed")
	}
	return result
}

func (f *operationFixture) mergeRequests() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string(nil), f.merges...)
}

func (f *operationFixture) state() (execution, task, approval, reason string) {
	f.t.Helper()
	if err := f.db.QueryRow(`SELECT e.status, t.status, a.status, coalesce(a.invalidated_reason,'')
		FROM executions e JOIN tasks t ON t.id=e.task_id JOIN approvals a ON a.execution_id=e.id WHERE a.id=?`, f.approvalID).
		Scan(&execution, &task, &approval, &reason); err != nil {
		f.t.Fatal(err)
	}
	return
}

// Acceptance (spec 17 9章): the test GitHub API receives exactly one merge
// request whose sha is content_version, and the merge completes the task.
// A later claim offers nothing more, so no second merge is requested.
func TestApprovedMergeSendsOneRequestWithContentVersionAndCompletesTask(t *testing.T) {
	f := newOperationFixture(t)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	merges := f.mergeRequests()
	if len(merges) != 1 || merges[0]["sha"] != f.content || merges[0]["merge_method"] != "merge" {
		t.Fatalf("merge requests = %v; want one with sha=%s", merges, f.content)
	}
	if execution, task, approval, _ := f.state(); execution != "succeeded" || task != "completed" || approval != "consumed" {
		t.Fatalf("state = %s/%s/%s; want succeeded/completed/consumed", execution, task, approval)
	}
	var mergeCommit string
	var uses int
	if err := f.db.QueryRow(`SELECT json_extract(result,'$.mergeCommitSha'), use_count FROM approvals WHERE id=?`, f.approvalID).Scan(&mergeCommit, &uses); err != nil ||
		mergeCommit != operationMergeCommit || uses != 1 {
		t.Fatalf("approval result %q uses %d %v", mergeCommit, uses, err)
	}
	claim, err := f.process.claim(context.Background())
	if err != nil || claim != nil {
		t.Fatalf("claim after merge = %+v %v; want no work", claim, err)
	}
	if merges := f.mergeRequests(); len(merges) != 1 {
		t.Fatalf("merge requests after reclaim = %d; want 1", len(merges))
	}
}

// Acceptance (spec 17 9章): when consume answers allowed=false, no merge
// request reaches GitHub. Each row is a refusal of the 3.5.2 table.
func TestRefusedConsumeSendsNoMergeRequest(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		prepare                  func(*operationFixture)
		execution, approval, why string
	}{
		{"head changed", func(f *operationFixture) { f.prHead = strings.Repeat("e", 40) }, "waiting_approval", "invalidated", "content_changed"},
		{"PR closed", func(f *operationFixture) { f.prOpen = false }, "waiting_approval", "invalidated", "target_changed"},
		{"expired", func(f *operationFixture) { f.approve(time.Now().Add(-time.Minute)) }, "waiting_approval", "expired", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOperationFixture(t)
			tc.prepare(f)
			if err := f.run(); err != nil {
				t.Fatal(err)
			}
			if merges := f.mergeRequests(); len(merges) != 0 {
				t.Fatalf("merge requests = %v; want none", merges)
			}
			if execution, task, approval, reason := f.state(); execution != tc.execution || task != "waiting_approval" || approval != tc.approval || reason != tc.why {
				t.Fatalf("state = %s/%s/%s/%s", execution, task, approval, reason)
			}
		})
	}
}

// Spec 17 6章: GitHub refuses the merge because the head no longer equals the
// sha parameter. The merge is not retried, the task fails and the consumed
// approval is invalidated (content_changed).
func TestMergeRefusedByGitHubForHeadMismatch(t *testing.T) {
	f := newOperationFixture(t)
	f.mergeCode = http.StatusConflict
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if merges := f.mergeRequests(); len(merges) != 1 || merges[0]["sha"] != f.content {
		t.Fatalf("merge requests = %v; want one", merges)
	}
	if execution, task, approval, reason := f.state(); execution != "failed" || task != "failed" || approval != "invalidated" || reason != "content_changed" {
		t.Fatalf("state = %s/%s/%s/%s", execution, task, approval, reason)
	}
	var class, message string
	if err := f.db.QueryRow(`SELECT error_class, error_message FROM executions WHERE id=?`, operationExecution).Scan(&class, &message); err != nil ||
		class != "publish_failed" || message != "content_changed" {
		t.Fatalf("error %s/%s %v", class, message, err)
	}
}

// Acceptance (spec 17 9章, 3.5.3): the connection of the merge request is cut
// before GitHub answers. The Git Publisher does not request the merge again;
// it reads the PR once more. A PR merged with content_version is a success,
// a PR still open is publish_failed (merge_unknown).
func TestUnknownMergeResultReadsPullRequestAgainWithoutSecondMerge(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		applies, unreadable      bool
		execution, task, message string
	}{
		{"merged", true, false, "succeeded", "completed", ""},
		{"still open", false, false, "failed", "failed", "merge_unknown"},
		{"unreadable", true, true, "failed", "failed", "merge_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOperationFixture(t)
			f.dropMerge, f.mergeApplies, f.failReread = true, tc.applies, tc.unreadable
			if err := f.run(); err != nil {
				t.Fatal(err)
			}
			if merges := f.mergeRequests(); len(merges) != 1 || merges[0]["sha"] != f.content {
				t.Fatalf("merge requests = %v; want exactly one", merges)
			}
			f.mu.Lock()
			reads := f.prReads
			f.mu.Unlock()
			// One read before consume (3.5.2 step 1) and one after the lost answer.
			if reads != 2 {
				t.Fatalf("PR reads = %d; want 2", reads)
			}
			if execution, task, approval, reason := f.state(); execution != tc.execution || task != tc.task || approval != "consumed" || reason != "" {
				t.Fatalf("state = %s/%s/%s/%s", execution, task, approval, reason)
			}
			var class, message sql.NullString
			var result string
			if err := f.db.QueryRow(`SELECT e.error_class, e.error_message, a.result FROM executions e JOIN approvals a ON a.execution_id=e.id WHERE a.id=?`, f.approvalID).
				Scan(&class, &message, &result); err != nil {
				t.Fatal(err)
			}
			if tc.message == "" {
				if class.String != "" || message.String != "" || !strings.Contains(result, operationMergeCommit) {
					t.Fatalf("success recorded as %q/%q result %s", class.String, message.String, result)
				}
			} else if class.String != "publish_failed" || message.String != tc.message || !strings.Contains(result, tc.message) {
				t.Fatalf("failure recorded as %q/%q result %s", class.String, message.String, result)
			}
			// A later claim offers nothing: the consumed approval is not reused.
			if claim, err := f.process.claim(context.Background()); err != nil || claim != nil {
				t.Fatalf("claim after merge = %+v %v; want no work", claim, err)
			}
			if merges := f.mergeRequests(); len(merges) != 1 {
				t.Fatalf("merge requests after reclaim = %d; want 1", len(merges))
			}
		})
	}
}
