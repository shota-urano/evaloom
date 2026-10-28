package store

import (
	"context"
	"database/sql"
	"strings"
)

// ListLogArtifacts excludes non-log artifacts and closes the query before the
// caller opens files through ArtifactStore (the DB has a single connection).
func ListLogArtifacts(ctx context.Context, db *sql.DB, executionID string) ([]Artifact, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, kind FROM artifacts
		WHERE execution_id = ? AND kind IN ('harness_log', 'setup_log', 'test_log')
		ORDER BY created_at, id`, executionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var artifacts []Artifact
	for rows.Next() {
		var artifact Artifact
		if err := rows.Scan(&artifact.ID, &artifact.Kind); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

// RunnerLog reads the already-redacted runner stream in event order. There is
// no runner_log artifact kind in the data model; other streams use their files.
func RunnerLog(ctx context.Context, db *sql.DB, executionID string) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT json_extract(payload, '$.text') FROM execution_events
		WHERE execution_id = ? AND type = 'log' AND json_extract(payload, '$.stream') = 'runner'
		ORDER BY id`, executionID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var text strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		text.WriteString(line)
		text.WriteByte('\n')
	}
	return text.String(), rows.Err()
}
