package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type projectReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const projectColumns = `id, name, runner_id, default_role,
	linear_team_id, linear_routing_label, created_at, updated_at`

func scanProject(row interface{ Scan(...any) error }) (model.Project, error) {
	var p model.Project
	err := row.Scan(&p.ID, &p.Name, &p.RunnerID, &p.DefaultRole,
		&p.LinearTeamID, &p.LinearRoutingLabel, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func GetProject(ctx context.Context, reader projectReader, id string) (model.Project, error) {
	return scanProject(reader.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
}

func RunnerWorktreeRoot(ctx context.Context, reader projectReader, id string) (string, error) {
	var root string
	err := reader.QueryRowContext(ctx, `SELECT worktree_root FROM runners WHERE id = ?`, id).Scan(&root)
	return root, err
}

func ListProjects(ctx context.Context, db *sql.DB, after string, limit int) ([]model.Project, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []model.Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

var ErrProjectConflict = errors.New("project ID already registered")

// SaveProject participates in the caller's transaction with its audit record.
func SaveProject(ctx context.Context, tx *sql.Tx, p model.Project, create bool) error {
	args := []any{p.Name, p.RunnerID, p.DefaultRole, p.LinearTeamID, p.LinearRoutingLabel, p.CreatedAt, p.UpdatedAt, p.ID}
	statement := `UPDATE projects SET name=?, runner_id=?, default_role=?,
		linear_team_id=?, linear_routing_label=?, created_at=?, updated_at=? WHERE id=?`
	if create {
		statement = `INSERT INTO projects (name, runner_id, default_role,
			linear_team_id, linear_routing_label, created_at, updated_at, id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	}
	_, err := tx.ExecContext(ctx, statement, args...)
	var constraint *sqlite.Error
	if errors.As(err, &constraint) && (constraint.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || constraint.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY) {
		return ErrProjectConflict
	}
	return err
}

// LinearRouteTaken reports whether a Project other than exceptID already
// registers the Team and routing label pair (spec 15 3.8,
// UNIQUE(linear_team_id, linear_routing_label)). It is read inside the
// saving transaction so the API can answer 409 with the field.
func LinearRouteTaken(ctx context.Context, tx *sql.Tx, teamID, label, exceptID string) (bool, error) {
	var taken bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM projects
		WHERE linear_team_id = ? AND linear_routing_label = ? AND id <> ?)`, teamID, label, exceptID).Scan(&taken)
	return taken, err
}
