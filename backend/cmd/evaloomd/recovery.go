package main

import (
	"context"
	"database/sql"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// recoverOnStartup is also callable with a controlled clock in tests. The
// daemon calls it only after serving the startup heartbeat grace period.
func recoverOnStartup(ctx context.Context, db *sql.DB, startedAt, now time.Time) error {
	return store.RecoverStaleRunners(ctx, db, startedAt, now)
}
