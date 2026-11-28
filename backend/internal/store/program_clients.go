package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// ErrProgramClientExists reports a duplicate program client ID (spec 20 4.3).
var ErrProgramClientExists = errors.New("store: program client already exists")

// ProgramClient is one program_clients row without its key hash. The hash is
// never returned to callers outside authentication.
type ProgramClient struct {
	ID, Name  string
	ProjectID string
	HarnessID string
	CreatedAt string
	RevokedAt *string
}

// RegisterProgramClient stores only the key hash, the routing target and an
// audit record, in one transaction (spec 20 4.3, 7章). The target must exist.
func RegisterProgramClient(ctx context.Context, db *sql.DB, client ProgramClient, tokenHash string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if client.ProjectID != "" {
		if _, err := GetProject(ctx, tx, client.ProjectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("program client: project %q is not registered", client.ProjectID)
			}
			return err
		}
	} else {
		if _, err := GetHarness(ctx, tx, client.HarnessID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("program client: harness %q is not registered", client.HarnessID)
			}
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO program_clients
		(id, name, token_hash, project_id, harness_id, created_at)
		VALUES (?, ?, ?, nullif(?, ''), nullif(?, ''), ?) ON CONFLICT(id) DO NOTHING`,
		client.ID, client.Name, tokenHash, client.ProjectID, client.HarnessID, client.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: register program client: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrProgramClientExists
	}
	detail := map[string]any{"name": client.Name}
	if client.ProjectID != "" {
		detail["projectId"] = client.ProjectID
	} else {
		detail["harnessId"] = client.HarnessID
	}
	if _, err := RecordAuditLog(ctx, tx, AuditLog{Actor: "owner", Action: "program_client.register",
		TargetType: "program_client", TargetID: client.ID, Detail: detail}); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeProgramClient sets revoked_at once. Revoking an unknown client is
// sql.ErrNoRows; revoking twice keeps the first time and succeeds.
func RevokeProgramClient(ctx context.Context, db *sql.DB, id string, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var revoked sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT revoked_at FROM program_clients WHERE id = ?`, id).Scan(&revoked); err != nil {
		return err
	}
	if revoked.Valid {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE program_clients SET revoked_at = ? WHERE id = ?`,
		now.UTC().Format(time.RFC3339), id); err != nil {
		return err
	}
	if _, err := RecordAuditLog(ctx, tx, AuditLog{Actor: "owner", Action: "program_client.revoke",
		TargetType: "program_client", TargetID: id}); err != nil {
		return err
	}
	return tx.Commit()
}

// ListProgramClients returns clients in ID order, without key hashes.
func ListProgramClients(ctx context.Context, db *sql.DB) ([]ProgramClient, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name, coalesce(project_id, ''), coalesce(harness_id, ''),
		created_at, revoked_at FROM program_clients ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var clients []ProgramClient
	for rows.Next() {
		var c ProgramClient
		if err := rows.Scan(&c.ID, &c.Name, &c.ProjectID, &c.HarnessID, &c.CreatedAt, &c.RevokedAt); err != nil {
			return nil, err
		}
		clients = append(clients, c)
	}
	return clients, rows.Err()
}

// ProgramClientByTokenHash resolves an active (not revoked) client. Unknown
// and revoked keys are sql.ErrNoRows; ambiguous rows fail closed.
func ProgramClientByTokenHash(ctx context.Context, db *sql.DB, hash string) (ProgramClient, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name, coalesce(project_id, ''), coalesce(harness_id, ''), created_at
		FROM program_clients WHERE token_hash = ? AND revoked_at IS NULL LIMIT 2`, hash)
	if err != nil {
		return ProgramClient{}, err
	}
	defer rows.Close()
	var found []ProgramClient
	for rows.Next() {
		var c ProgramClient
		if err := rows.Scan(&c.ID, &c.Name, &c.ProjectID, &c.HarnessID, &c.CreatedAt); err != nil {
			return ProgramClient{}, err
		}
		found = append(found, c)
	}
	if err := rows.Err(); err != nil {
		return ProgramClient{}, err
	}
	switch len(found) {
	case 0:
		return ProgramClient{}, sql.ErrNoRows
	case 1:
		return found[0], nil
	}
	return ProgramClient{}, fmt.Errorf("store: ambiguous program client identity")
}

// InsertProgramTask inserts a task with source=program for clientID inside
// the caller's creation transaction (spec 20 4.4).
func InsertProgramTask(ctx context.Context, tx *sql.Tx, t *model.Task, clientID string) error {
	if err := allocateTaskID(ctx, tx, t); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO tasks
		(id, project_id, harness_id, title, body, acceptance_criteria,
		collaboration_mode, status, idempotency_key, created_at, updated_at, source, program_client_id)
		VALUES (?, nullif(?, ''), nullif(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, 'program', ?)`, t.ID, t.ProjectID, t.HarnessID,
		t.Title, t.Body, t.AcceptanceCriteria, t.CollaborationMode, t.Status,
		t.IdempotencyKey, t.CreatedAt, t.UpdatedAt, clientID)
	return err
}

// ProgramTaskCounts returns the client's unfinished tasks and the tasks it
// created at or after since (spec 20 4.6).
func ProgramTaskCounts(ctx context.Context, tx *sql.Tx, clientID string, since time.Time) (open, today int, err error) {
	err = tx.QueryRowContext(ctx, `SELECT
		coalesce(sum(CASE WHEN status NOT IN ('completed', 'failed', 'timed_out', 'cancelled') THEN 1 ELSE 0 END), 0),
		coalesce(sum(CASE WHEN created_at >= ? THEN 1 ELSE 0 END), 0)
		FROM tasks WHERE program_client_id = ?`, since.UTC().Format(time.RFC3339), clientID).Scan(&open, &today)
	return open, today, err
}

// ProgramTask is what a program may read about its own task (spec 20 4.5).
type ProgramTask struct {
	ID, Status, CreatedAt, UpdatedAt string
	Execution                        *ProgramExecution
}

type ProgramExecution struct {
	ID           string
	Seq          int
	Status       string
	ErrorClass   *string
	ErrorMessage *string
}

// GetProgramTask reads the task only when clientID created it. Any other task
// is sql.ErrNoRows, so callers cannot tell it exists.
func GetProgramTask(ctx context.Context, db *sql.DB, clientID, taskID string) (ProgramTask, error) {
	var t ProgramTask
	err := db.QueryRowContext(ctx, `SELECT id, status, created_at, updated_at FROM tasks
		WHERE id = ? AND source = 'program' AND program_client_id = ?`, taskID, clientID).
		Scan(&t.ID, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return ProgramTask{}, err
	}
	var e ProgramExecution
	err = db.QueryRowContext(ctx, `SELECT id, seq, status, error_class, error_message FROM executions
		WHERE task_id = ? ORDER BY seq DESC LIMIT 1`, taskID).Scan(&e.ID, &e.Seq, &e.Status, &e.ErrorClass, &e.ErrorMessage)
	if err == nil {
		t.Execution = &e
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ProgramTask{}, err
	}
	return t, nil
}

// ProgramFile is one file a program may take out (spec 20 4.5).
type ProgramFile struct {
	ArtifactID, Name, SHA256 string
	SizeBytes                int64
}

// ListProgramFiles returns the check stage's out files and the work summary
// (named summary.md) of executionID. For a name registered more than once the
// latest registration wins, and a check stage file wins over summary.md.
func ListProgramFiles(ctx context.Context, db *sql.DB, executionID string) ([]ProgramFile, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, stage, kind, name, sha256, size_bytes FROM artifacts
		WHERE execution_id = ? AND ((stage = 'check' AND kind = 'out_file') OR (stage = 'work' AND kind = 'work_summary'))
		ORDER BY created_at, rowid`, executionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]ProgramFile{}
	fromCheck := map[string]bool{}
	var order []string
	for rows.Next() {
		var f ProgramFile
		var stage, kind string
		if err := rows.Scan(&f.ArtifactID, &stage, &kind, &f.Name, &f.SHA256, &f.SizeBytes); err != nil {
			return nil, err
		}
		check := stage == "check"
		if kind == "work_summary" {
			f.Name = "summary.md"
		}
		if _, seen := byName[f.Name]; !seen {
			order = append(order, f.Name)
		} else if fromCheck[f.Name] && !check {
			continue
		}
		byName[f.Name] = f
		fromCheck[f.Name] = fromCheck[f.Name] || check
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	files := make([]ProgramFile, 0, len(order))
	for _, name := range order {
		files = append(files, byName[name])
	}
	return files, nil
}
