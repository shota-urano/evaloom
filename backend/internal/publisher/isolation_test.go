package publisher_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/publisher"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Only this test executable routes the fixed github.com Git destination to a
// local TLS smart-HTTP fixture and trusts its certificate. Production keeps
// HTTPS, certificate verification, the fixed destination and no redirects.
func TestPublisherGitHelper(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 {
		return
	}
	realGit, endpoint, ca, record := os.Args[i+1], os.Args[i+2], os.Args[i+3], os.Args[i+4]
	args := os.Args[i+5:]
	env := os.Environ()
	if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--git-dir=") }) {
		expected := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+credentialCanary))
		if os.Getenv("GIT_CONFIG_VALUE_0") != expected || os.Getenv("GIT_CONFIG_COUNT") != "1" ||
			os.Getenv("GIT_CONFIG_KEY_0") != "http.https://github.com/owner/repo.git.extraHeader" ||
			os.Getenv("GIT_ALLOW_PROTOCOL") != "https" || os.Getenv("GITHUB_TOKEN") != "" || os.Getenv("GIT_TRACE_CURL") != "" ||
			strings.Contains(strings.Join(args, "\n"), credentialCanary) || strings.Contains(strings.Join(args, "\n"), expected) ||
			!slices.Contains(args, "http.followRedirects=false") || !slices.Contains(args, "http.sslVerify=true") {
			os.Exit(80)
		}
		for i, arg := range args {
			if arg == "https://github.com/owner/repo.git" {
				args[i] = endpoint + "/remote.git"
			}
		}
		for i, value := range env {
			if strings.HasPrefix(value, "GIT_CONFIG_KEY_0=") {
				env[i] = "GIT_CONFIG_KEY_0=http." + endpoint + "/remote.git.extraHeader"
			}
		}
		args = append([]string{"-c", "http.sslCAInfo=" + ca}, args...)
		file, err := os.OpenFile(record, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(81)
		}
		_, _ = fmt.Fprintln(file, "authenticated Git invocation")
		_ = file.Close()
	}
	cmd := exec.Command(realGit, args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		os.Exit(82)
	}
	os.Exit(0)
}

// This fake runtime records the actual Container.Run argv/environment. It is
// not an OS isolation test; spec 14's real Sandbox probes remain separate.
func TestPublisherSandboxHelper(t *testing.T) {
	path := os.Getenv("EVALOOM_SANDBOX_RECORD")
	if path == "" {
		return
	}
	i := slices.Index(os.Args, "--")
	args := os.Args[i+1:]
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(83)
	}
	_ = json.NewEncoder(file).Encode(struct{ Args, Env []string }{args, os.Environ()})
	_ = file.Close()
	if args[0] == "image" {
		fmt.Print(`{"Os":"linux","Config":{}}`)
	} else if args[1] == "inspect" {
		fmt.Print(`{"Status":"exited","Running":false,"ExitCode":0}`)
	} else if args[1] == "start" {
		fmt.Print("sandbox probe ran\n")
	}
	os.Exit(0)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func helperScript(t *testing.T, path, testName string, args ...string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := "#!/bin/sh\nexport GORACE=atexit_sleep_ms=0\nexec " + shellQuote(self) + " -test.run=^" + testName + "$ --"
	for _, arg := range args {
		command += " " + shellQuote(arg)
	}
	if err := os.WriteFile(path, []byte(command+" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func assertNoCredentials(t *testing.T, label string, data []byte, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(data, []byte(secret)) {
			t.Fatalf("credential found in %s", label)
		}
	}
}

func scanFiles(t *testing.T, root string, secrets []string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assertNoCredentials(t, path, data, secrets)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type sqlitePublicationRecorder struct{ db *sql.DB }

func (r sqlitePublicationRecorder) RecordPushed(ctx context.Context, sha, branch string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE executions SET pushed_sha=? WHERE id='execution-1'`, sha)
	if err == nil {
		_, err = r.db.ExecContext(ctx, `INSERT INTO execution_events(execution_id,type,payload,created_at) VALUES('execution-1','pushed',?,'now')`, `{"sha":"`+sha+`","branch":"`+branch+`"}`)
	}
	return err
}

func (r sqlitePublicationRecorder) RecordPR(ctx context.Context, pr publisher.PullRequest, created bool) error {
	_, err := r.db.ExecContext(ctx, `UPDATE executions SET pr_number=?,pr_url=? WHERE id='execution-1'`, pr.Number, pr.URL)
	if err == nil && created {
		data, _ := json.Marshal(map[string]any{"prNumber": pr.Number, "prUrl": pr.URL})
		_, err = r.db.ExecContext(ctx, `INSERT INTO execution_events(execution_id,type,payload,created_at) VALUES('execution-1','pr_created',?,'now')`, string(data))
	}
	return err
}

func publicationDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, query := range []string{
		`INSERT INTO runners(id,name,token_hash,worktree_root,status,last_heartbeat_at,created_at) VALUES('runner-1','test','hash','/worktrees','online','now','now')`,
		`INSERT INTO projects(id,name,local_repo_path,git_remote_url,runner_id,base_branch,default_role,context_location,copy_allowlist,allowed_operations,approval_policy,timeouts,created_at,updated_at) VALUES('project-1','test','/repo','https://github.com/owner/repo','runner-1','main','development','repo','[]','[]','{}','{}','now','now')`,
		`INSERT INTO tasks(id,project_id,title,body,acceptance_criteria,short_name,task_type,collaboration_mode,status,idempotency_key,created_at,updated_at) VALUES('task-1','project-1','test','','publish','short','development','solo','running','test','now','now')`,
		`INSERT INTO executions(id,task_id,seq,mode,status,created_at) VALUES('execution-1','task-1',1,'fresh','publishing','now')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func scanDB(t *testing.T, db *sql.DB, secrets []string) {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		values, pointers := make([]any, len(cols)), make([]any, len(cols))
		for i := range pointers {
			pointers[i] = &values[i]
		}
		for rows.Next() {
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for i, value := range values {
				assertNoCredentials(t, table+"."+cols[i], []byte(fmt.Sprint(value)), secrets)
				if raw, ok := value.([]byte); ok {
					assertNoCredentials(t, table+"."+cols[i], raw, secrets)
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
}

func TestPublisherCredentialsNeverReachWorktreeSandboxLogsOrDB(t *testing.T) {
	key, keyPEM := appKey(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"success", "comment", "key_error", "token_error", "git_rejection", "git_redirect", "api_error", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			managed := filepath.Dir(f.worktree) // Includes clone/.git and linked-worktree metadata.
			private := t.TempDir()
			secrets := []string{credentialCanary, string(keyPEM), strings.Split(string(keyPEM), "\n")[1], base64.StdEncoding.EncodeToString([]byte("x-access-token:" + credentialCanary))}
			remote := filepath.Join(private, "remote.git")
			f.git(private, "init", "--bare", remote)
			f.git(remote, "config", "http.receivepack", "true")
			if scenario == "git_rejection" {
				hook := filepath.Join(remote, "hooks", "pre-receive")
				f.write(hook, "#!/bin/sh\necho '"+credentialCanary+"' >&2\nexit 1\n")
				if err := os.Chmod(hook, 0700); err != nil {
					t.Fatal(err)
				}
			}
			var gitRequests atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				username, password, ok := r.BasicAuth()
				if !ok || username != "x-access-token" || password != credentialCanary {
					t.Error("Git request did not use installation authentication")
					http.Error(w, "unauthenticated", 401)
					return
				}
				gitRequests.Add(1)
				if scenario == "git_redirect" {
					if r.URL.Path == "/leak" {
						t.Error("Git followed an authenticated redirect")
					}
					w.Header().Set("Location", "/leak?token="+credentialCanary)
					w.WriteHeader(307)
					return
				}
				cmd := exec.Command(realGit, "http-backend")
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
					"GIT_PROJECT_ROOT=" + private, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=publisher", "REQUEST_METHOD=" + r.Method,
					"PATH_INFO=" + r.URL.Path, "QUERY_STRING=" + r.URL.RawQuery, "CONTENT_TYPE=" + r.Header.Get("Content-Type")}
				cmd.Stdin = r.Body
				out, err := cmd.Output()
				if err != nil {
					t.Error("local Git HTTP backend failed")
					w.WriteHeader(500)
					return
				}
				reader := bufio.NewReader(bytes.NewReader(out))
				headers, err := textproto.NewReader(reader).ReadMIMEHeader()
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				status := 200
				for name, values := range headers {
					if name == "Status" {
						status, _ = strconv.Atoi(strings.Fields(values[0])[0])
					} else {
						w.Header()[name] = values
					}
				}
				w.WriteHeader(status)
				_, _ = io.Copy(w, reader)
			}))
			defer server.Close()
			ca := filepath.Join(private, "ca.pem")
			f.write(ca, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})))
			gitRecord := filepath.Join(managed, "git-invocations")
			helperScript(t, filepath.Join(private, "git"), "TestPublisherGitHelper", realGit, server.URL, ca, gitRecord)
			t.Setenv("PATH", private+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GITHUB_TOKEN", credentialCanary)
			t.Setenv("GIT_TRACE_CURL", filepath.Join(managed, "git-trace"))
			// These settings must never reach the credential-bearing Git process.
			f.git(f.worktree, "config", "http.https://github.com/.proxy", "http://127.0.0.1:1")
			f.git(f.worktree, "config", "http.https://github.com/.sslVerify", "false")
			f.git(f.worktree, "config", "http.cookieFile", filepath.Join(managed, "cookies"))
			f.git(f.worktree, "config", "http.saveCookies", "true")
			f.git(f.worktree, "config", "credential.helper", "!env > '"+filepath.Join(managed, "helper-env")+"'")
			f.git(f.worktree, "config", "core.hooksPath", filepath.Join(managed, "hooks"))
			f.write(filepath.Join(managed, "hooks", "pre-push"), "#!/bin/sh\nenv > '"+filepath.Join(managed, "hook-env")+"'\n")
			if err := os.Chmod(filepath.Join(managed, "hooks", "pre-push"), 0700); err != nil {
				t.Fatal(err)
			}
			beforeConfig, err := os.ReadFile(filepath.Join(managed, "clone", ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}
			dbDir := t.TempDir()
			db := publicationDB(t, filepath.Join(dbDir, "evaloom.db"))
			var logs bytes.Buffer
			standardLog := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(standardLog) })
			output, err := os.Create(filepath.Join(managed, "publisher-output.log"))
			if err != nil {
				t.Fatal(err)
			}
			stdout, stderr := os.Stdout, os.Stderr
			os.Stdout, os.Stderr = output, output
			t.Cleanup(func() { os.Stdout, os.Stderr = stdout, stderr; _ = output.Close() })
			logger := log.New(&logs, "publisher ", 0)
			keyReads, tokenRequests, apiRequests := 0, 0, 0
			g, err := publisher.NewGitHubApp(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasPrefix(r.URL.Path, "/app/") {
					tokenRequests++
					secrets = append(secrets, checkTokenRequest(t, r, key))
					scanFiles(t, managed, secrets)
					if scenario == "token_error" {
						return reply(401, credentialCanary), nil
					}
					return reply(201, tokenResponse()), nil
				}
				apiRequests++
				if r.Header.Get("Authorization") != "Bearer "+credentialCanary {
					t.Fatal("REST request did not use installation authentication")
				}
				if scenario == "api_error" {
					return nil, errors.New("transport echoed " + credentialCanary)
				}
				if r.Method == "GET" {
					if scenario == "comment" {
						return reply(200, `[`+prJSON+`]`), nil
					}
					return reply(200, `[]`), nil
				}
				if strings.HasSuffix(r.URL.Path, "/comments") {
					return reply(201, `{}`), nil
				}
				return reply(201, prJSON), nil
			})}, "ssh://git@github.com/owner/repo.git", func(context.Context) ([]byte, error) {
				keyReads++
				if scenario == "key_error" {
					return nil, errors.New("key loader echoed " + credentialCanary)
				}
				return keyPEM, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			config := publisher.PublishConfig{Push: publisher.PushConfig{RepositoryURL: "git@github.com:owner/repo.git", Git: g,
				CheckCancellation: func(context.Context) error {
					if scenario == "cancelled" {
						cancel()
					}
					return nil
				},
				RecordRemoteMismatch: func(context.Context, string, string) error { return errors.New("unexpected mismatch") }},
				BaseBranch: "main", PullRequests: g, Recorder: sqlitePublicationRecorder{db}, ReportContext: ctx, Report: executionReport(f.head)}
			result, publishErr := publisher.Publish(ctx, f.target(), config)
			logger.Printf("publication=%+v error=%v", result, publishErr)
			message := ""
			if publishErr != nil {
				message = publishErr.Error()
			}
			if _, err := db.Exec(`UPDATE executions SET error_class=?,error_message=? WHERE id='execution-1'`, nullableClass(publishErr), message); err != nil {
				t.Fatal(err)
			}
			if scenario == "success" || scenario == "comment" {
				if publishErr != nil || result.PR == nil || result.PushedSHA != f.head || keyReads != 3 || tokenRequests != 3 || apiRequests != 2 || gitRequests.Load() < 3 {
					t.Fatalf("authentication/publication did not complete: err=%v key=%d token=%d api=%d git=%d", publishErr, keyReads, tokenRequests, apiRequests, gitRequests.Load())
				}
				if got := f.git(remote, "rev-parse", "refs/heads/"+f.target().BranchName); got != f.head {
					t.Fatal("authenticated push did not publish verified SHA")
				}
				var sha, prURL string
				if err := db.QueryRow(`SELECT pushed_sha,pr_url FROM executions`).Scan(&sha, &prURL); err != nil || sha != f.head || prURL != "https://github.com/owner/repo/pull/17" {
					t.Fatal("publication was not durably recorded")
				}
			} else if scenario == "cancelled" {
				if !errors.Is(publishErr, context.Canceled) || keyReads != 1 || tokenRequests != 1 || gitRequests.Load() != 0 || apiRequests != 0 {
					t.Fatal("cancellation was not rechecked after credential acquisition")
				}
			} else if publishErr == nil || publisher.ErrorClass(publishErr) != "publish_failed" {
				t.Fatal("failure was not classified as publish_failed")
			}
			probePublisherSandbox(t, f, private, &logs, secrets)
			assertNoCredentials(t, "logs", logs.Bytes(), secrets)
			scanFiles(t, managed, secrets)
			scanDB(t, db, secrets)
			scanFiles(t, dbDir, secrets) // Includes SQLite WAL and SHM, before checkpoint.
			afterConfig, err := os.ReadFile(filepath.Join(managed, "clone", ".git", "config"))
			if err != nil || !bytes.Equal(beforeConfig, afterConfig) {
				t.Fatal("Publisher rewrote worktree Git configuration")
			}
			for _, name := range []string{"hook-env", "helper-env", "cookies", "git-trace"} {
				if _, err := os.Stat(filepath.Join(managed, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("untrusted credential sink was used: %s", name)
				}
			}
			if os.Getenv("GITHUB_TOKEN") != credentialCanary {
				t.Fatal("Publisher mutated parent environment")
			}
		})
	}
}

func nullableClass(err error) any {
	if err == nil {
		return nil
	}
	return string(publisher.ErrorClass(err))
}

func probePublisherSandbox(t *testing.T, f *fixture, private string, logs io.Writer, secrets []string) {
	t.Helper()
	record := filepath.Join(filepath.Dir(f.worktree), "sandbox-calls.jsonl")
	runtime := filepath.Join(private, "sandbox-runtime")
	configDir := filepath.Join(filepath.Dir(f.worktree), "sandbox-config")
	if err := os.Mkdir(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	helperScript(t, runtime, "TestPublisherSandboxHelper")
	container, err := sandbox.NewContainer(sandbox.ContainerConfig{RuntimePath: runtime, Image: "probe", ControlTimeout: 5 * time.Second,
		RuntimeEnv: map[string]string{"EVALOOM_SANDBOX_RECORD": record}, AllowEnv: []string{"PROBE"},
		WorktreeRoot: f.worktree, GitDirRoot: filepath.Join(filepath.Dir(f.worktree), "clone"), ConfigDirRoot: configDir,
		HostHome: private, DataDir: filepath.Join(private, "data"), RunnerTokenFile: filepath.Join(private, "runner-token"), Stdout: logs, Stderr: logs})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []sandbox.SandboxKind{sandbox.Harness, sandbox.TestSetup, sandbox.TestRun} {
		configMount := ""
		if kind == sandbox.Harness {
			configMount = configDir
		}
		process, err := container.Run(context.Background(), sandbox.SandboxSpec{Kind: kind, Worktree: f.worktree,
			GitDir: filepath.Join(filepath.Dir(f.worktree), "clone"), ConfigDir: configMount, Env: map[string]string{"PROBE": "observed"},
			Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 1 << 20, Time: 10 * time.Second}}, []string{"env"})
		if err != nil {
			t.Fatalf("Sandbox environment probe could not start: %v", err)
		}
		if err := process.Wait(); err != nil {
			t.Fatalf("Sandbox environment probe failed: %v", err)
		}
	}
	data, err := os.ReadFile(record)
	if err != nil || !bytes.Contains(data, []byte("PROBE=observed")) || !bytes.Contains(data, []byte("-i")) {
		t.Fatal("Sandbox invocation was not observed")
	}
	assertNoCredentials(t, "Sandbox argv and environment", data, secrets)
}
