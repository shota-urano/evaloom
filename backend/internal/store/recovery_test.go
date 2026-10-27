package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestBootIdentityMigrationPreservesExistingRunnerAndPersistsHeartbeat(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "evaloom.db")
	registrySQL, err := migrationFiles.ReadFile("migrations/0001_registry.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := open(ctx, path, fstest.MapFS{"0001_registry.sql": {Data: registrySQL}})
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	var boot sql.NullString
	var hash, root string
	if err := db.QueryRow(`SELECT boot_id, token_hash, worktree_root FROM runners WHERE id = 'mac-mini'`).Scan(&boot, &hash, &root); err != nil {
		t.Fatal(err)
	}
	if boot.Valid {
		t.Fatal("migration invented a previous boot identity")
	}
	if _, err := Heartbeat(ctx, db, "mac-mini", "persisted-boot", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	var gotBoot, gotHash, gotRoot, status, heartbeat string
	if err := db.QueryRow(`SELECT boot_id, token_hash, worktree_root, status, last_heartbeat_at FROM runners WHERE id = 'mac-mini'`).Scan(&gotBoot, &gotHash, &gotRoot, &status, &heartbeat); err != nil {
		t.Fatal(err)
	}
	if gotBoot != "persisted-boot" || gotHash != hash || gotRoot != root || status != "online" || heartbeat == "" {
		t.Fatalf("Runner not preserved across migration/reopen: boot=%s status=%s heartbeat=%s", gotBoot, status, heartbeat)
	}
}
