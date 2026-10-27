package store

import (
	"context"
	"database/sql"
	"strings"

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

// ListProjectHarnesses returns the pinned Profile of every role for the given
// Projects, with the Profile's version and commit SHA, ordered by Project and role.
func ListProjectHarnesses(ctx context.Context, db *sql.DB, projectIDs []string) ([]model.ProjectHarnessView, error) {
	views := []model.ProjectHarnessView{}
	if len(projectIDs) == 0 {
		return views, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(projectIDs)), ", ")
	args := make([]any, len(projectIDs))
	for i, id := range projectIDs {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, `SELECT ph.project_id, ph.role, ph.harness_id, ph.profile_id,
		hp.version, hp.commit_sha, ph.assigned_at
		FROM project_harnesses ph JOIN harness_profiles hp ON hp.id = ph.profile_id
		WHERE ph.project_id IN (`+placeholders+`) ORDER BY ph.project_id, ph.role`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v model.ProjectHarnessView
		if err := rows.Scan(&v.ProjectID, &v.Role, &v.HarnessID, &v.ProfileID, &v.Version, &v.CommitSHA, &v.AssignedAt); err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, rows.Err()
}
