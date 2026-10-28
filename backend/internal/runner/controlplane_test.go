package runner

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Exercise the production registration, bearer authentication and heartbeat
// receiver together, including IDs that require URL path escaping.
func TestHeartbeatAcceptedByControlPlane(t *testing.T) {
	ctx := context.Background()
	db, config, _ := controlPlaneFixture(t, nil)
	var previousBoot string
	for range 2 {
		process, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := process.Heartbeat(ctx); err != nil {
				t.Fatal(err)
			}
		}
		var boot, status, lastHeartbeat string
		if err := db.QueryRow(`SELECT boot_id, status, last_heartbeat_at FROM runners WHERE id = ?`, config.RunnerID).Scan(&boot, &status, &lastHeartbeat); err != nil {
			t.Fatal(err)
		}
		if boot != process.bootID || boot == previousBoot || status != "online" || lastHeartbeat == "" {
			t.Fatalf("heartbeat not persisted correctly: boot=%q status=%s time=%s", boot, status, lastHeartbeat)
		}
		previousBoot = boot
	}
}

func controlPlaneFixture(t *testing.T, observe func(*http.Request, int)) (*sql.DB, Config, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth, err := api.NewOwnerAuth(db, logger, server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	handler := api.NewHandler(db, logger, auth)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if observe == nil {
			handler.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		w.Write(recorder.Body.Bytes())
		observe(r, recorder.Code)
	})
	server.Start()
	config := testConfig(t, server.URL)
	config.RunnerID = "mac%?#日本語"
	token, err := runnerauth.Register(ctx, db, config.RunnerID, "Mac mini", config.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.TokenFile, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return db, config, server
}

// Repeated real authenticated claims must create neither DB rows nor local
// handlers. This checks the no-work contract across both sides of HTTP.
func TestPullNoWorkCreatesNoExecutions(t *testing.T) {
	attempts := make(chan int, 128)
	db, config, _ := controlPlaneFixture(t, func(r *http.Request, status int) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			attempts <- status
		}
	})
	process, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	process.pollInterval = time.Millisecond
	var started atomic.Int32
	startPull(t, process, func(context.Context, Claim) { started.Add(1) }, func(err error) { t.Error(err) })
	for range 10 {
		if status := await(t, attempts); status != http.StatusNoContent {
			t.Fatalf("empty queue returned HTTP %d", status)
		}
	}
	for _, table := range []string{"executions", "execution_events", "tasks"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("empty claims changed %s: count=%d err=%v", table, count, err)
		}
	}
	if started.Load() != 0 {
		t.Fatalf("empty claims started %d execution handlers", started.Load())
	}
}
