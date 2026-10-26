package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"modernc.org/sqlite"
	"modernc.org/sqlite/lib"
)

const harnessColumns = `id, name, specialty, supported_tasks, adapter, repo_path,
	repo_url, supported_models, available_tools, tool_permissions, contract_version, created_at, updated_at`

func scanHarness(row interface{ Scan(...any) error }) (model.Harness, error) {
	var h model.Harness
	var tasks, models, tools, permissions string
	err := row.Scan(&h.ID, &h.Name, &h.Specialty, &tasks, &h.Adapter, &h.RepoPath,
		&h.RepoURL, &models, &tools, &permissions, &h.ContractVersion, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		return h, err
	}
	for _, column := range []struct {
		data string
		dst  any
	}{{tasks, &h.SupportedTasks}, {models, &h.SupportedModels},
		{tools, &h.AvailableTools}, {permissions, &h.ToolPermissions}} {
		decoder := json.NewDecoder(strings.NewReader(column.data))
		decoder.UseNumber()
		if err := decoder.Decode(column.dst); err != nil {
			return h, err
		}
	}
	return h, nil
}

func GetHarness(ctx context.Context, reader projectReader, id string) (model.Harness, error) {
	return scanHarness(reader.QueryRowContext(ctx, `SELECT `+harnessColumns+` FROM harnesses WHERE id = ?`, id))
}

func ListHarnesses(ctx context.Context, db *sql.DB, after string, limit int) ([]model.Harness, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+harnessColumns+` FROM harnesses WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	harnesses := []model.Harness{}
	for rows.Next() {
		h, err := scanHarness(rows)
		if err != nil {
			return nil, err
		}
		harnesses = append(harnesses, h)
	}
	return harnesses, rows.Err()
}

type queryReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func repositoryPaths(ctx context.Context, reader queryReader, query string, args ...any) ([]string, error) {
	rows, err := reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := []string{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

// A Harness is shared, so exclude every registered Runner's Worktree Root.
func RunnerWorktreeRoots(ctx context.Context, reader queryReader) ([]string, error) {
	return repositoryPaths(ctx, reader, `SELECT DISTINCT worktree_root FROM runners ORDER BY worktree_root`)
}

func HarnessRepositoryPaths(ctx context.Context, tx *sql.Tx, exceptID string) ([]string, error) {
	return repositoryPaths(ctx, tx, `SELECT repo_path FROM harnesses WHERE id <> ?`, exceptID)
}

var ErrHarnessConflict = errors.New("harness ID or repository path already registered")

// SaveHarness participates in the caller's transaction with its audit record.
func SaveHarness(ctx context.Context, tx *sql.Tx, h model.Harness, create bool) error {
	args := []any{h.Name, h.Specialty, h.Adapter, h.RepoPath, h.RepoURL, h.ContractVersion}
	for _, value := range []any{h.SupportedTasks, h.SupportedModels, h.AvailableTools, h.ToolPermissions} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		args = append(args, string(encoded))
	}
	args = append(args, h.CreatedAt, h.UpdatedAt, h.ID)
	statement := `UPDATE harnesses SET name=?, specialty=?, adapter=?, repo_path=?, repo_url=?, contract_version=?,
		supported_tasks=?, supported_models=?, available_tools=?, tool_permissions=?, created_at=?, updated_at=? WHERE id=?`
	if create {
		statement = `INSERT INTO harnesses (name, specialty, adapter, repo_path, repo_url, contract_version,
			supported_tasks, supported_models, available_tools, tool_permissions, created_at, updated_at, id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	}
	_, err := tx.ExecContext(ctx, statement, args...)
	var constraint *sqlite.Error
	if errors.As(err, &constraint) && (constraint.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || constraint.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY) {
		return ErrHarnessConflict
	}
	return err
}
