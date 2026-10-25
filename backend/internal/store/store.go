// Package store owns the Control Plane's SQLite connection and migrations.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations
var migrationFiles embed.FS

// Open opens a file-backed database, enables WAL, and applies pending embedded
// migrations before returning. The caller owns Close. Parent directories must
// already exist; an empty path or an in-memory database is not supported.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: migration directory: %w", err)
	}
	return open(ctx, path, source)
}

func open(ctx context.Context, path string, source fs.FS) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" {
		return nil, fmt.Errorf("store: a database file path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: database path: %w", err)
	}
	// Treat the supplied path as a filename, never as a caller-provided SQLite
	// URI. Apply foreign_keys to every connection, including replacements.
	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := url.Values{"_pragma": {"foreign_keys(1)"}}
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	// The Control Plane is the sole writer. Serialize operations on one
	// connection to avoid competing writers within the process.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: enable WAL: %w", err)
	}
	if mode != "wal" {
		db.Close()
		return nil, fmt.Errorf("store: expected WAL journal mode, got %q", mode)
	}
	if err := migrate(ctx, db, source); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
