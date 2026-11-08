package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"modernc.org/sqlite"
	"modernc.org/sqlite/lib"
)

type projectReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const projectColumns = `id, name, local_repo_path, git_remote_url, runner_id,
	base_branch, default_role, review_role, context_location, test_command,
	test_report_format, test_report_path, setup_command, setup_allowed_hosts, copy_allowlist,
	allowed_operations, approval_policy, timeouts, linear_team_id, linear_routing_label,
	created_at, updated_at`

func scanProject(row interface{ Scan(...any) error }) (model.Project, error) {
	var p model.Project
	var hosts, copies, operations, policy, timeouts string
	err := row.Scan(&p.ID, &p.Name, &p.LocalRepoPath, &p.GitRemoteURL, &p.RunnerID,
		&p.BaseBranch, &p.DefaultRole, &p.ReviewRole, &p.ContextLocation, &p.TestCommand,
		&p.TestReportFormat, &p.TestReportPath, &p.SetupCommand, &hosts, &copies,
		&operations, &policy, &timeouts, &p.LinearTeamID, &p.LinearRoutingLabel, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	for _, column := range []struct {
		data string
		dst  any
	}{{hosts, &p.SetupAllowedHosts}, {copies, &p.CopyAllowlist},
		{operations, &p.AllowedOperations}, {policy, &p.ApprovalPolicy}, {timeouts, &p.Timeouts}} {
		if !json.Valid([]byte(column.data)) {
			return p, fmt.Errorf("store: invalid stored Project JSON")
		}
		decoder := json.NewDecoder(strings.NewReader(column.data))
		decoder.UseNumber()
		if err := decoder.Decode(column.dst); err != nil {
			return p, err
		}
	}
	return p, nil
}

func GetProject(ctx context.Context, reader projectReader, id string) (model.Project, error) {
	return scanProject(reader.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
}

func RunnerWorktreeRoot(ctx context.Context, reader projectReader, id string) (string, error) {
	var root string
	err := reader.QueryRowContext(ctx, `SELECT worktree_root FROM runners WHERE id = ?`, id).Scan(&root)
	return root, err
}

// ProjectRepositoryPaths is read inside the mutation transaction for filesystem
// identity checks in addition to the database's path-string UNIQUE constraint.
func ProjectRepositoryPaths(ctx context.Context, tx *sql.Tx, exceptID string) ([]string, error) {
	return repositoryPaths(ctx, tx, `SELECT local_repo_path FROM projects WHERE id <> ?`, exceptID)
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

var ErrProjectConflict = errors.New("project ID or repository path already registered")

// SaveProject participates in the caller's transaction with its audit record.
// The UNIQUE path constraint remains authoritative under concurrent requests.
func SaveProject(ctx context.Context, tx *sql.Tx, p model.Project, create bool) error {
	args := []any{p.Name, p.LocalRepoPath, p.GitRemoteURL, p.RunnerID, p.BaseBranch,
		p.DefaultRole, p.ReviewRole, p.ContextLocation, p.TestCommand,
		p.TestReportFormat, p.TestReportPath, p.SetupCommand}
	for _, value := range []any{p.SetupAllowedHosts, p.CopyAllowlist, p.AllowedOperations, p.ApprovalPolicy, p.Timeouts} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		args = append(args, string(encoded))
	}
	args = append(args, p.LinearTeamID, p.LinearRoutingLabel, p.CreatedAt, p.UpdatedAt, p.ID)
	statement := `UPDATE projects SET name=?, local_repo_path=?, git_remote_url=?, runner_id=?,
		base_branch=?, default_role=?, review_role=?, context_location=?, test_command=?,
		test_report_format=?, test_report_path=?, setup_command=?, setup_allowed_hosts=?, copy_allowlist=?,
		allowed_operations=?, approval_policy=?, timeouts=?,
		linear_team_id=?, linear_routing_label=?, created_at=?, updated_at=? WHERE id=?`
	if create {
		statement = `INSERT INTO projects (name, local_repo_path, git_remote_url, runner_id,
			base_branch, default_role, review_role, context_location, test_command,
			test_report_format, test_report_path, setup_command, setup_allowed_hosts, copy_allowlist,
			allowed_operations, approval_policy, timeouts, linear_team_id, linear_routing_label,
			created_at, updated_at, id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
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
