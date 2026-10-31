package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func TestRunInitializesDatabaseBeforeWaitingForShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output := make(startupOutput, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-db", path, "-port", "0"}, output) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()

	// Avoid opening a competing reader while startup switches to WAL.
	select {
	case <-output:
	case <-ctx.Done():
		t.Fatal("server did not initialize before the deadline")
	}

	// Opening read-only avoids creating a file that would hide a missing
	// startup call. Observe the database actually initialized by evaloomd.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil || count == 0 {
		t.Fatalf("database was not initialized: count=%d err=%v", count, err)
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("startup journal_mode = %q, err = %v; want wal", mode, err)
	}
}

func TestListenLoopbackBindsOnlyIPv4Loopback(t *testing.T) {
	listener, err := listenLoopback(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address type = %T; want *net.TCPAddr", listener.Addr())
	}
	if !address.IP.IsLoopback() || address.IP.To4() == nil || !address.IP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("bound IP = %v; want exactly IPv4 127.0.0.1", address.IP)
	}
	if address.Port == 0 {
		t.Fatal("listener did not acquire a port")
	}
}

type startupOutput chan string

func (output startupOutput) Write(p []byte) (int, error) {
	output <- string(p)
	return len(p), nil
}

func TestRunServesAfterMigrationAndClosesListenerOnShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output := make(startupOutput, 1)
	stopped := make(chan struct{})
	var runErr error
	go func() {
		runErr = run(ctx, []string{"-db", path, "-port", "0"}, output)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
			if runErr != nil {
				t.Errorf("run: %v", runErr)
			}
		case <-time.After(2 * shutdownTimeout):
			t.Error("run did not stop after cancellation")
		}
	})
	var line string
	select {
	case line = <-output:
	case <-stopped:
		t.Fatalf("server exited before startup: %v", runErr)
	case <-ctx.Done():
		t.Fatal("server did not start")
	}
	lines := strings.Split(strings.TrimSpace(line), "\n")
	address, ok := strings.CutPrefix(lines[0], "evaloomd: listening on ")
	if !ok {
		t.Fatalf("unexpected startup output: %q", line)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("running server address = %q, err = %v; want 127.0.0.1", address, err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("HTTP started before migrations succeeded: %v", err)
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport, Timeout: time.Second, Jar: jar}
	loginURL, ok := strings.CutPrefix(lines[1], "evaloomd: owner login URL: ")
	if !ok {
		t.Fatalf("missing login URL: %q", line)
	}
	login, err := client.Get(loginURL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, login.Body)
	login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", login.StatusCode)
	}
	response, err := client.Get("http://" + address + "/")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unregistered route status = %d; want 404", response.StatusCode)
	}
	response, err = client.Get("http://" + address + "/api/runners?limit=1")
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor string            `json:"nextCursor"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&page)
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("running API: status=%d page=%+v err=%v", response.StatusCode, page, decodeErr)
	}
	transport.CloseIdleConnections()
	// Keep an actual SSE request open during shutdown. Without cancellation
	// inherited from the daemon, Shutdown waits for its timeout and fails.
	writer, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_, seedErr := writer.Exec(`
		INSERT INTO runners (id, name, token_hash, worktree_root, status, last_heartbeat_at, created_at)
		VALUES ('runner', 'Runner', 'unused', '/worktrees', 'offline', '', '2026-10-26T00:00:00Z');
		INSERT INTO projects (id, name, local_repo_path, git_remote_url, runner_id, base_branch,
		default_role, context_location, copy_allowlist, allowed_operations,
		approval_policy, timeouts, created_at, updated_at)
		VALUES ('project', 'Project', '/repo', '', 'runner', 'main', 'developer', 'AGENTS.md',
		'[]', '{}', '{}', '{}', '2026-10-26T00:00:00Z', '2026-10-26T00:00:00Z');
		INSERT INTO tasks (id, project_id, title, body, acceptance_criteria, short_name, task_type,
		collaboration_mode, status, idempotency_key, created_at, updated_at)
		VALUES ('task-001', 'project', 'Title', '', 'events resume', 'sse', 'development', 'solo',
		'queued', 'stream-test', '2026-10-26T00:00:00Z', '2026-10-26T00:00:00Z');
		INSERT INTO executions (id, task_id, seq, mode, status, created_at)
		VALUES ('01ARZ3NDEKTSV4RRFFQ69G5FAV', 'task-001', 1, 'fresh', 'pending', '2026-10-26T00:00:00Z');`)
	closeErr := writer.Close()
	if seedErr != nil || closeErr != nil {
		t.Fatalf("seed Execution: %v; close: %v", seedErr, closeErr)
	}
	stream, err := client.Get("http://" + address + "/api/executions/01ARZ3NDEKTSV4RRFFQ69G5FAV/events")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != 200 || stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE before shutdown: status=%d headers=%v", stream.StatusCode, stream.Header)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * shutdownTimeout):
		t.Fatal("run did not stop after cancellation")
	}
	if body, err := io.ReadAll(stream.Body); err != nil || len(body) != 0 {
		t.Fatalf("SSE did not end cleanly on shutdown: body=%q err=%v", body, err)
	}
	connection, err := net.DialTimeout("tcp4", address, time.Second)
	if err == nil {
		connection.Close()
		t.Fatal("listener still accepts connections after shutdown")
	}
}

func TestRunRejectsOccupiedPort(t *testing.T) {
	listener, err := listenLoopback(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = run(ctx, []string{"-db", filepath.Join(t.TempDir(), "evaloom.db"), "-port", strconv.Itoa(port)}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("run with occupied port = %v; want listen error", err)
	}
}

func TestRunRejectsInvalidPortBeforeCreatingDatabase(t *testing.T) {
	for _, port := range []string{"-1", "65536", "0.0.0.0:8080", "[::]:8080"} {
		t.Run(port, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evaloom.db")
			if err := run(context.Background(), []string{"-db", path, "-port", port}, io.Discard); err == nil {
				t.Fatal("run succeeded with invalid port")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid configuration created database: %v", err)
			}
		})
	}
}

func TestRunRejectsInvalidDatabaseConfiguration(t *testing.T) {
	for _, args := range [][]string{nil, {"-db", t.TempDir()}, {"-db", "unused.db", "unexpected"}} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := run(ctx, args, io.Discard)
		cancel()
		if err == nil {
			t.Fatalf("run(%v) succeeded; want startup error", args)
		}
	}
}

func TestRunRejectsWebDirectoryWithoutIndex(t *testing.T) {
	dir := t.TempDir()
	err := run(context.Background(), []string{"-db", filepath.Join(t.TempDir(), "evaloom.db"), "-port", "0", "-web", dir}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("run with empty web directory: %v; want index.html error", err)
	}
}

// Acceptance (13-management-ui 9): evaloomd serves the built UI on its own origin.
func TestRunServesBuiltManagementUI(t *testing.T) {
	web := t.TempDir()
	const index = `<!doctype html><title>Evaloom</title><div id="root"></div>`
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output := make(startupOutput, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-db", filepath.Join(t.TempDir(), "evaloom.db"), "-port", "0", "-web", web}, output)
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	var line string
	select {
	case line = <-output:
	case err := <-done:
		t.Fatalf("server exited before startup: %v", err)
	}
	address, ok := strings.CutPrefix(strings.Split(line, "\n")[0], "evaloomd: listening on ")
	if !ok {
		t.Fatalf("unexpected startup output: %q", line)
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	get := func(path string) (int, string) {
		response, err := client.Get("http://" + address + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(body)
	}
	for _, path := range []string{"/", "/tasks/task-001"} {
		if status, body := get(path); status != http.StatusOK || body != index {
			t.Fatalf("GET %s = %d %q; want UI index", path, status, body)
		}
	}
	if status, body := get("/api/runners"); status != http.StatusUnauthorized || strings.Contains(body, "<title>") {
		t.Fatalf("GET /api/runners without session = %d %q; want 401 JSON", status, body)
	}
}
