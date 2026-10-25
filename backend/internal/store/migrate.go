package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

type migration struct {
	version int64
	name    string
	sql     string
}

func migrate(ctx context.Context, db *sql.DB, source fs.FS) error {
	migrations, err := readMigrations(source)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: initialize migration history: %w", err)
	}
	for _, migration := range migrations {
		if err := applyMigration(ctx, db, migration); err != nil {
			return fmt.Errorf("store: migration %s: %w", migration.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, migration migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name string
	err = tx.QueryRowContext(ctx, "SELECT name FROM schema_migrations WHERE version = ?", migration.version).Scan(&name)
	if err == nil {
		if name != migration.name {
			return fmt.Errorf("version %d was already applied as %s", migration.version, name)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, migration.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
		migration.version, migration.name, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

func readMigrations(source fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return nil, fmt.Errorf("store: list migrations: %w", err)
	}
	var migrations []migration
	versions := make(map[int64]bool)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		name := entry.Name()
		prefix, description, ok := strings.Cut(strings.TrimSuffix(name, ".sql"), "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if !ok || description == "" || err != nil || version <= 0 || strings.Trim(prefix, "0123456789") != "" {
			return nil, fmt.Errorf("store: invalid migration name %q (expected <positive version>_<name>.sql)", name)
		}
		if versions[version] {
			return nil, fmt.Errorf("store: duplicate migration version %d", version)
		}
		versions[version] = true
		content, err := fs.ReadFile(source, name)
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", name, err)
		}
		if strings.TrimSpace(string(content)) == "" {
			return nil, fmt.Errorf("store: empty migration %s", name)
		}
		migrations = append(migrations, migration{version: version, name: name, sql: string(content)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	return migrations, nil
}
