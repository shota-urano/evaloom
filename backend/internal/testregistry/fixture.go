// Package testregistry supplies registration dependencies for lifecycle tests.
package testregistry

import (
	"context"
	"database/sql"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func Seed(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO harnesses (id,name,repo_url,repo_path,created_at,updated_at)
 VALUES ('fixture-harness','Fixture','https://example.invalid/h.git','/fixture/h','2026-11-10T00:00:00Z','2026-11-10T00:00:00Z');
 INSERT OR IGNORE INTO harness_profiles (id,harness_id,version,commit_sha,tag_name,contract_version,definition,definition_hash,model_id,operation_status,registered_at)
 VALUES ('fixture-profile','fixture-harness','1.0.0','sha','profile/v1.0.0','1','{"executionConfiguration":{"adapter":"codex"}}','hash','model','provisional','2026-11-10T00:00:00Z')`)
	return err
}

func Open(ctx context.Context, path string) (*sql.DB, error) {
	db, err := store.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := Seed(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
