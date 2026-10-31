package runner

import (
	"bufio"
	"context"
	"database/sql"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const logExecution = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

type logControlPlane struct {
	t                        *testing.T
	db                       *sql.DB
	files                    *store.ArtifactStore
	config                   Config
	server                   *httptest.Server
	cookie                   *http.Cookie
	dbPath, dataDir, address string
	loseEventAck             atomic.Bool
	loseArtifactAck          atomic.Bool
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
		`INSERT INTO projects (id,name,local_repo_path,git_remote_url,runner_id,base_branch,default_role,context_location,copy_allowlist,allowed_operations,approval_policy,timeouts,created_at,updated_at)
		VALUES ('project','Project','/repo','','mac-mini','main','developer','AGENTS.md','[]','{}','{}','{}','now','now')`,
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
	f.files, err = store.NewArtifactStore(f.db, f.dataDir)
	if err != nil {
		f.t.Fatal(err)
	}
	handler := api.NewHandlerWithArtifacts(f.db, logger, auth, nil, api.ContextConfig{}, api.ArtifactConfig{Store: f.files, MaxBytes: 1 << 20})
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lose := r.Method == "POST" && (strings.HasSuffix(r.URL.Path, "/events") && f.loseEventAck.Swap(false) || strings.HasSuffix(r.URL.Path, "/artifacts") && f.loseArtifactAck.Swap(false))
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
	if f.db != nil {
		f.files.Close()
		f.files = nil
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

// This stub records the registration boundary without defining an HTTP API
// or accessing the Control Plane DB from the Runner.
type recordingArtifactRegistrar struct {
	mu       sync.Mutex
	artifact LogArtifact
	content  string
	calls    int
	failures int
}

func (registrar *recordingArtifactRegistrar) RegisterLogArtifact(ctx context.Context, artifact LogArtifact) error {
	registrar.mu.Lock()
	defer registrar.mu.Unlock()
	registrar.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if registrar.failures > 0 {
		registrar.failures--
		return fmt.Errorf("registration temporarily unavailable")
	}
	content, err := os.ReadFile(artifact.Path)
	if err != nil {
		return err
	}
	registrar.artifact, registrar.content = artifact, string(content)
	return nil
}

func assertLogArtifact(t *testing.T, registrar *recordingArtifactRegistrar, log *Log, want string) {
	t.Helper()
	registrar.mu.Lock()
	defer registrar.mu.Unlock()
	artifact := registrar.artifact
	if registrar.calls != 1 || artifact.ID != log.id || artifact.ExecutionID != logExecution || artifact.Kind != log.stream+"_log" || artifact.Path != log.Path() || artifact.Redacted || registrar.content != want {
		t.Fatalf("registration calls=%d artifact=%+v content=%q", registrar.calls, artifact, registrar.content)
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
	registrar := &recordingArtifactRegistrar{}
	log, err := process.NewLog(logExecution, "harness", registrar)
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
	assertLogArtifact(t, registrar, log, "first\n\nstderr line\nlast without newline\n")
}

func TestLogsPersistDuringControlPlaneStopAndResendAfterRestart(t *testing.T) {
	f := newLogControlPlane(t)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	registrar := &recordingArtifactRegistrar{}
	log, err := process.NewLog(logExecution, "setup", registrar)
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
	f.stop() // Stop HTTP and close SQLite, not a mocked 503.
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
	assertLogArtifact(t, registrar, log, want)
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE type='log'`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("events=%d err=%v", count, err)
	}
}

func TestLogRetryAfterLostAcknowledgementsIsIdempotent(t *testing.T) {
	f := newLogControlPlane(t)
	f.loseEventAck.Store(true)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	registrar := &recordingArtifactRegistrar{}
	log, err := process.NewLog(logExecution, "test", registrar)
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
	for table, want := range map[string]int{"execution_events": 2, "artifacts": 0} {
		var got int
		if err := f.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s=%d want=%d err=%v", table, got, want, err)
		}
	}
	assertLogArtifact(t, registrar, log, "one\ntwo\n")
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
	want := []string{
		fmt.Sprintf(`{"sourceId":"%s:0","stream":"test","text":"one"}`, log.id),
		fmt.Sprintf(`{"sourceId":"%s:4","stream":"test","text":"two"}`, log.id),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(fmt.Sprintf("log events=%v want=%v", got, want))
	}
}

func TestLogArtifactRegistrationRetriesOnlyAfterFinish(t *testing.T) {
	f := newLogControlPlane(t)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	registrar := &recordingArtifactRegistrar{failures: 2}
	log, err := process.NewLog(logExecution, "test", registrar)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Finish(true) })
	done := startLogDelivery(t, log)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, reader := f.stream(ctx)
	if err := log.Read(strings.NewReader("test output\n")); err != nil {
		t.Fatal(err)
	}
	readLogFrame(t, reader, "test", "test output")
	registrar.mu.Lock()
	beforeFinish := registrar.calls
	registrar.mu.Unlock()
	if beforeFinish != 0 {
		t.Fatalf("registered an open log: calls=%d", beforeFinish)
	}
	if err := log.Finish(true); err != nil {
		t.Fatal(err)
	}
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
	if registrar.calls != 3 || !registrar.artifact.Redacted || registrar.content != "test output\n" {
		t.Fatalf("registration retries: %+v", registrar)
	}
	if err := log.Deliver(ctx); err != nil || registrar.calls != 3 {
		t.Fatalf("repeated delivery: calls=%d err=%v", registrar.calls, err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE type='log'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("registration retry resent events: count=%d err=%v", count, err)
	}
}

func TestLogDeliveryResumesWithoutDuplicatesAfterControlPlaneRestart(t *testing.T) {
	f := newLogControlPlane(t)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	registrar := &recordingArtifactRegistrar{}
	log, err := process.NewLog(logExecution, "harness", registrar)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Finish(false) })
	log.retryInterval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deliveryCtx, stopDelivery := context.WithCancel(ctx)
	defer stopDelivery()
	done := make(chan error, 1)
	go func() { done <- log.Deliver(deliveryCtx) }()
	response, reader := f.stream(ctx)
	if err := log.Read(strings.NewReader("before restart\n")); err != nil {
		t.Fatal(err)
	}
	readLogFrame(t, reader, "harness", "before restart")
	stopDelivery()
	if err := await(t, done); err != context.Canceled {
		t.Fatalf("stopped delivery: %v", err)
	}
	response.Body.Close()
	f.stop()
	if err := log.Read(strings.NewReader("after restart\n")); err != nil {
		t.Fatal(err)
	}
	if err := log.Finish(false); err != nil {
		t.Fatal(err)
	}
	f.start()
	// A new Deliver call replays from byte zero. Deduplication must survive
	// reopening the DB and must not depend on an in-memory receipt cache.
	if err := log.Deliver(ctx); err != nil {
		t.Fatal(err)
	}
	_, reader = f.stream(ctx)
	readLogFrame(t, reader, "harness", "before restart")
	readLogFrame(t, reader, "harness", "after restart")
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM execution_events WHERE type='log'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("events after replay=%d err=%v", count, err)
	}
	assertLogArtifact(t, registrar, log, "before restart\nafter restart\n")
}
