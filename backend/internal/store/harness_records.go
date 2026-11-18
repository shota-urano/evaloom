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
	SHA256                                         string
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
	rows, err := db.QueryContext(ctx, `SELECT hr.id, hr.harness_id, hr.artifact_id, hr.execution_id, hr.task_id,
		hr.source_identifier, hr.file_name, hr.created_at, hr.deleted_at, a.sha256
		FROM harness_records hr JOIN artifacts a ON a.id = hr.artifact_id
		WHERE hr.harness_id = ? AND hr.deleted_at IS NULL
		ORDER BY hr.created_at DESC, hr.id DESC LIMIT ?`, harnessID, recent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []HarnessRecord{}
	for rows.Next() {
		var record HarnessRecord
		if err := rows.Scan(&record.ID, &record.HarnessID, &record.ArtifactID, &record.ExecutionID,
			&record.TaskID, &record.SourceIdentifier, &record.FileName, &record.CreatedAt, &record.DeletedAt, &record.SHA256); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
