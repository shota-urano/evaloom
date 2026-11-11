package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type projectReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const projectColumns = `id, name, harness_id, profile_id, params, approval_waivers, runner_id, created_at, updated_at`

func scanProject(row interface{ Scan(...any) error }) (model.Project, error) {
	var p model.Project
	var params, waivers string
	err := row.Scan(&p.ID, &p.Name, &p.HarnessID, &p.ProfileID, &params, &waivers, &p.RunnerID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	p.Params = json.RawMessage(params)
	err = json.Unmarshal([]byte(waivers), &p.ApprovalWaivers)
	return p, err
}

func GetProject(ctx context.Context, reader projectReader, id string) (model.Project, error) {
	return scanProject(reader.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
}

func RunnerExists(ctx context.Context, reader projectReader, id string) error {
	var found string
	return reader.QueryRowContext(ctx, `SELECT id FROM runners WHERE id = ?`, id).Scan(&found)
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
	waivers, err := json.Marshal(p.ApprovalWaivers)
	if err != nil {
		return err
	}
	args := []any{p.Name, p.HarnessID, p.ProfileID, string(p.Params), string(waivers), p.RunnerID, p.CreatedAt, p.UpdatedAt, p.ID}
	statement := `UPDATE projects SET name=?, harness_id=?, profile_id=?, params=?, approval_waivers=?, runner_id=?, created_at=?, updated_at=? WHERE id=?`
	if create {
		statement = `INSERT INTO projects (name, harness_id, profile_id, params, approval_waivers, runner_id, created_at, updated_at, id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	}
	_, err = tx.ExecContext(ctx, statement, args...)
	var constraint *sqlite.Error
	if errors.As(err, &constraint) && (constraint.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || constraint.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY) {
		return ErrProjectConflict
	}
	return err
}
