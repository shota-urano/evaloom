package runner

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const logExecution = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

type logControlPlane struct {
	t                             *testing.T
	db                            *sql.DB
	config                        Config
	server                        *httptest.Server
	artifacts                     *store.ArtifactStore
	cookie                        *http.Cookie
	dbPath, dataDir, address      string
	loseEventAck, loseArtifactAck atomic.Bool
}

func newLogControlPlane(t *testing.T) *logControlPlane {
	t.Helper()
	f := &logControlPlane{t: t, dataDir: t.TempDir()}
	f.dbPath = filepath.Join(f.dataDir, "evaloom.db")
	f.start()
	f.config = testConfig(t, f.server.URL)
	token, err := runnerauth.Register(context.Background(), f.db, f.config.RunnerID, "Runner", f.config.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config.TokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO projects (id,name,local_repo_path,git_remote_url,runner_id,base_branch,default_role,context_location,copy_allowlist,test_file_patterns,allowed_operations,approval_policy,timeouts,created_at,updated_at)
		VALUES ('project','Project','/repo','','mac-mini','main','developer','AGENTS.md','[]','[]','{}','{}','{}','now','now')`,
		`INSERT INTO tasks (id,project_id,title,body,acceptance_criteria,short_name,task_type,collaboration_mode,status,idempotency_key,created_at,updated_at)
		VALUES ('task-001','project','Title','','logs','logs','development','solo','running','logs','now','now')`,
		`INSERT INTO executions (id,task_id,seq,mode,status,runner_id,created_at)
		VALUES ('` + logExecution + `','task-001',1,'fresh','running','mac-mini','now')`,
	} {
		if _, err := f.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(f.stop)
	return f
}

func (f *logControlPlane) start() {
	f.t.Helper()
	var err error
	f.db, err = store.Open(context.Background(), f.dbPath)
	if err != nil {
		f.t.Fatal(err)
	}
	f.artifacts, err = store.NewArtifactStore(f.db, f.dataDir)
	if err != nil {
		f.t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	if f.address != "" {
		server.Listener.Close()
		server.Listener, err = net.Listen("tcp4", f.address)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	f.address = server.Listener.Addr().String()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth, err := api.NewOwnerAuth(f.db, logger, f.address)
	if err != nil {
		f.t.Fatal(err)
	}
	handler := api.NewHandlerWithArtifacts(f.db, logger, auth, nil, api.ContextConfig{}, f.artifacts)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lose := r.Method == "POST" && ((strings.HasSuffix(r.URL.Path, "/events") && f.loseEventAck.Swap(false)) ||
			(strings.Contains(r.URL.Path, "/artifacts/") && f.loseArtifactAck.Swap(false)))
		if !lose {
			handler.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		if recorder.Code != 204 {
			f.t.Errorf("report before lost ack: %d %s", recorder.Code, recorder.Body.String())
		}
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			f.t.Error(err)
			return
		}
		connection.Close() // Commit succeeded, but the Runner receives no response.
	})
	server.Start()
	f.server = server
	response, err := server.Client().Get(auth.LoginURL())
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || len(response.Cookies()) != 1 {
		f.t.Fatal("owner login failed")
	}
	f.cookie = response.Cookies()[0]
}

func (f *logControlPlane) stop() {
	if f.server != nil {
		f.server.CloseClientConnections()
		f.server.Close()
		f.server = nil
	}
	if f.artifacts != nil {
		f.artifacts.Close()
		f.artifacts = nil
	}
	if f.db != nil {
		f.db.Close()
		f.db = nil
	}
}

func (f *logControlPlane) stream(ctx context.Context) (*http.Response, *bufio.Reader) {
	f.t.Helper()
	r, err := http.NewRequestWithContext(ctx, "GET", f.server.URL+"/api/executions/"+logExecution+"/events", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	r.AddCookie(f.cookie)
	response, err := f.server.Client().Do(r)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { response.Body.Close() })
	if response.StatusCode != 200 {
		f.t.Fatalf("SSE returned %d", response.StatusCode)
	}
	return response, bufio.NewReader(response.Body)
}

func readLogFrame(t *testing.T, reader *bufio.Reader, stream, text string) {
	t.Helper()
	var id, event string
	var payload struct {
		Stream string `json:"stream"`
		Text   string `json:"text"`
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\n" {
			break
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimSpace(strings.TrimPrefix(line, "id: "))
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	if id == "" || event != "log" || payload.Stream != stream || payload.Text != text {
		t.Fatalf("unexpected SSE log: id=%q event=%q payload=%+v; want %s %q", id, event, payload, stream, text)
	}
}

func startLogDelivery(t *testing.T, log *Log) <-chan error {
	t.Helper()
	log.retryInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan error, 1)
	go func() { done <- log.Deliver(ctx) }()
	t.Cleanup(cancel)
	return done
}

func assertLogArtifact(t *testing.T, f *logControlPlane, log *Log, want string) {
	t.Helper()
	artifact, file, err := f.artifacts.Open(context.Background(), logExecution, log.id)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(want))
	if string(content) != want || artifact.Kind != log.stream+"_log" || artifact.SizeBytes != int64(len(want)) || artifact.SHA256 != hex.EncodeToString(hash[:]) || artifact.Redacted {
		t.Fatalf("artifact mismatch: %+v content=%q", artifact, content)
	}
	local, err := os.ReadFile(log.Path())
	if err != nil || string(local) != want {
		t.Fatalf("local log=%q err=%v", local, err)
	}
	info, err := os.Stat(log.Path())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("log permissions must be 0600")
	}
}

func TestHarnessLogReachesControlPlaneSSEWhileOutputIsRunning(t *testing.T) {
	f := newLogControlPlane(t)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	log, err := process.NewLog(logExecution, "harness")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Finish(false) })
	done := startLogDelivery(t, log)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, reader := f.stream(ctx)
	stdout, write := io.Pipe()
	defer stdout.Close()
	defer write.Close()
	readDone := make(chan error, 1)
	go func() { readDone <- log.Read(stdout) }()
	if _, err := io.WriteString(write, "first\r\n\n"); err != nil {
		t.Fatal(err)
	}
	readLogFrame(t, reader, "harness", "first") // EOF has not occurred yet.
	readLogFrame(t, reader, "harness", "")
	if err := log.Read(strings.NewReader("stderr line\n")); err != nil {
		t.Fatal(err)
	}
	readLogFrame(t, reader, "harness", "stderr line")
	if _, err := io.WriteString(write, "last without newline"); err != nil {
		t.Fatal(err)
	}
	write.Close()
	if err := await(t, readDone); err != nil {
		t.Fatal(err)
	}
	if err := log.Finish(false); err != nil {
		t.Fatal(err)
	}
	readLogFrame(t, reader, "harness", "last without newline")
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
	assertLogArtifact(t, f, log, "first\n\nstderr line\nlast without newline\n")
}

func TestLogsPersistDuringControlPlaneStopAndResendAfterRestart(t *testing.T) {
	f := newLogControlPlane(t)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	log, err := process.NewLog(logExecution, "setup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Finish(false) })
	done := startLogDelivery(t, log)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, reader := f.stream(ctx)
	if err := log.Read(strings.NewReader("before stop\n")); err != nil {
		t.Fatal(err)
	}
	readLogFrame(t, reader, "setup", "before stop")
	response.Body.Close()
	f.stop() // Stop HTTP and close SQLite and artifact storage, not a mocked 503.
	longLine := strings.Repeat("x", 128<<10)
	want := "before stop\nwhile offline\n" + longLine + "\nafter EOF\n"
	if err := log.Read(strings.NewReader("while offline\n" + longLine + "\nafter EOF")); err != nil {
		t.Fatal(err)
	}
	if err := log.Finish(false); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(log.Path())
	if err != nil || string(content) != want {
		t.Fatalf("log not written during outage: len=%d err=%v", len(content), err)
	}
	select {
	case err := <-done:
		t.Fatalf("delivery ended during outage: %v", err)
	default:
	}
	f.start() // Reopen the same DB at the same URL, with fresh owner credentials.
	_, reader = f.stream(ctx)
	for _, text := range []string{"before stop", "while offline", longLine, "after EOF"} {
		readLogFrame(t, reader, "setup", text)
	}
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
	assertLogArtifact(t, f, log, want)
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE type='log'`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("events=%d err=%v", count, err)
	}
}

func TestLogRetryAfterLostAcknowledgementsIsIdempotent(t *testing.T) {
	f := newLogControlPlane(t)
	f.loseEventAck.Store(true)
	f.loseArtifactAck.Store(true)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	log, err := process.NewLog(logExecution, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Read(strings.NewReader("one\ntwo\n")); err != nil {
		t.Fatal(err)
	}
	if err := log.Finish(false); err != nil {
		t.Fatal(err)
	}
	if err := await(t, startLogDelivery(t, log)); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"execution_events": 2, "runner_event_receipts": 2, "artifacts": 1} {
		var got int
		if err := f.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s=%d want=%d err=%v", table, got, want, err)
		}
	}
	files, err := filepath.Glob(filepath.Join(f.dataDir, "artifacts", "*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("artifact files=%v err=%v", files, err)
	}
	assertLogArtifact(t, f, log, "one\ntwo\n")
	rows, err := f.db.Query(`SELECT payload FROM execution_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		got = append(got, payload)
	}
	want := []string{`{"stream":"test","text":"one"}`, `{"stream":"test","text":"two"}`}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(fmt.Sprintf("log events=%v want=%v", got, want))
	}
}
