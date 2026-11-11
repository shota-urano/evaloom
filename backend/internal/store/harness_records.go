package store

import (
	"context"
	"database/sql"
	"fmt"
)

// HarnessRecord refers to an out_file artifact kept across Profile versions.
type HarnessRecord struct {
	ID, HarnessID, ArtifactID, ExecutionID, TaskID string
	SourceIdentifier                               *string
	FileName, CreatedAt                            string
	DeletedAt                                      *string
}

// HarnessRecordsForNextWork selects the input for a new context snapshot
// (spec 07, section 3.3). The caller supplies records.pass.recent and may use
// its context-pinning transaction. Existing snapshots are not changed here.
func HarnessRecordsForNextWork(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, harnessID string, recent int) ([]HarnessRecord, error) {
	if recent < 0 || recent > 50 {
		return nil, fmt.Errorf("store: records.pass.recent must be between 0 and 50")
	}
	rows, err := db.QueryContext(ctx, `SELECT id, harness_id, artifact_id, execution_id, task_id,
		source_identifier, file_name, created_at, deleted_at FROM harness_records
		WHERE harness_id = ? AND deleted_at IS NULL
		ORDER BY created_at DESC, id DESC LIMIT ?`, harnessID, recent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []HarnessRecord{}
	for rows.Next() {
		var record HarnessRecord
		if err := rows.Scan(&record.ID, &record.HarnessID, &record.ArtifactID, &record.ExecutionID,
			&record.TaskID, &record.SourceIdentifier, &record.FileName, &record.CreatedAt, &record.DeletedAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
