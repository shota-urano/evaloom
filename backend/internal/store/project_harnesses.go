package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// GetProjectHarness is the existing execution-input boundary. The role argument
// is ignored: spec 02 assigns one immutable Profile directly to the Project.
func GetProjectHarness(ctx context.Context, reader projectReader, projectID, _ string) (model.ProjectHarness, error) {
	var a model.ProjectHarness
	err := reader.QueryRowContext(ctx, `SELECT id, harness_id, profile_id, updated_at FROM projects WHERE id=?`, projectID).
		Scan(&a.ProjectID, &a.HarnessID, &a.ProfileID, &a.AssignedAt)
	return a, err
}

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
	rows, err := db.QueryContext(ctx, `SELECT p.id, p.harness_id, p.profile_id, hp.version, hp.commit_sha, p.updated_at
 FROM projects p JOIN harness_profiles hp ON hp.id=p.profile_id WHERE p.id IN (`+placeholders+`) ORDER BY p.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v model.ProjectHarnessView
		if err := rows.Scan(&v.ProjectID, &v.HarnessID, &v.ProfileID, &v.Version, &v.CommitSHA, &v.AssignedAt); err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, rows.Err()
}
