package store

import (
	"context"
	"database/sql"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func GetProjectHarness(ctx context.Context, reader projectReader, projectID, role string) (model.ProjectHarness, error) {
	var assignment model.ProjectHarness
	err := reader.QueryRowContext(ctx, `SELECT project_id, role, harness_id, profile_id, assigned_at
		FROM project_harnesses WHERE project_id = ? AND role = ?`, projectID, role).
		Scan(&assignment.ProjectID, &assignment.Role, &assignment.HarnessID, &assignment.ProfileID, &assignment.AssignedAt)
	return assignment, err
}

// SaveProjectHarness replaces only this Project role's pin. The caller resolves
// the Profile and records the assignment history in the same transaction.
func SaveProjectHarness(ctx context.Context, tx *sql.Tx, assignment model.ProjectHarness) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO project_harnesses
		(project_id, role, harness_id, profile_id, assigned_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(project_id, role) DO UPDATE SET harness_id = excluded.harness_id,
		profile_id = excluded.profile_id, assigned_at = excluded.assigned_at`,
		assignment.ProjectID, assignment.Role, assignment.HarnessID, assignment.ProfileID, assignment.AssignedAt)
	return err
}
