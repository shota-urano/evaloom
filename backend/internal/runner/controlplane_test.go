package runner

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Exercise the production registration, bearer authentication and heartbeat
// receiver together, including IDs that require URL path escaping.
func TestHeartbeatAcceptedByControlPlane(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "evaloom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := httptest.NewUnstartedServer(nil)
	defer server.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth, err := api.NewOwnerAuth(db, logger, server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = api.NewHandler(db, logger, auth)
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
