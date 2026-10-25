package main

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestRunInitializesDatabaseBeforeWaitingForShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-db", path}, io.Discard) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()

	// Opening read-only avoids creating a file that would hide a missing
	// startup call. Observe the database actually initialized by evaloomd.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var count int
		err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count)
		if err == nil {
			var mode string
			if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
				t.Fatalf("startup journal_mode = %q, err = %v; want wal", mode, err)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("database was not initialized: %v", err)
		case <-ticker.C:
		}
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
