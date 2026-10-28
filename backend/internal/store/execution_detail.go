package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

type ExecutionDetail struct {
	TaskExecution
	TaskID                               string
	RunnerID, Role, HarnessID, ProfileID *string
	ContextRevisions                     []ExecutionContextRevision
	GateResults                          []ExecutionGateResult
}

// ExecutionContextRevision deliberately excludes the pinned document body.
type ExecutionContextRevision struct {
	ID, Layer, Kind, SourcePath, SourceCommitSHA, ContentHash string
}

type ExecutionGateResult struct {
	GateResult
	CreatedAt string
}

// GetExecutionDetail reads only saved pins and results, in one snapshot. In
// particular it never resolves the project's current Harness assignment.
func GetExecutionDetail(ctx context.Context, db *sql.DB, id string) (ExecutionDetail, error) {
	result := ExecutionDetail{}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT id, task_id, seq, mode, status, error_class,
		profile_version, profile_commit_sha, started_at, finished_at, pr_url,
		runner_id, role, harness_id, profile_id FROM executions WHERE id = ?`, id).Scan(
		&result.ID, &result.TaskID, &result.Seq, &result.Mode, &result.Status, &result.ErrorClass,
		&result.ProfileVersion, &result.ProfileCommitSHA, &result.StartedAt, &result.FinishedAt,
		&result.PRURL, &result.RunnerID, &result.Role, &result.HarnessID, &result.ProfileID)
	if err != nil {
		return result, err
	}
	result.ContextRevisions, err = listExecutionContextMetadata(ctx, tx, id)
	if err == nil {
		result.GateResults, err = listExecutionGateResults(ctx, tx, id)
	}
	if err == nil {
		err = tx.Commit()
	}
	return result, err
}

func listExecutionContextMetadata(ctx context.Context, tx *sql.Tx, id string) ([]ExecutionContextRevision, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, layer, kind, source_path, source_commit_sha, content_hash
		FROM context_revisions WHERE execution_id = ?
		ORDER BY CASE layer WHEN 'global' THEN 0 WHEN 'harness' THEN 1 WHEN 'project' THEN 2 ELSE 3 END,
		source_path, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ExecutionContextRevision{}
	for rows.Next() {
		var revision ExecutionContextRevision
		if err := rows.Scan(&revision.ID, &revision.Layer, &revision.Kind, &revision.SourcePath,
			&revision.SourceCommitSHA, &revision.ContentHash); err != nil {
			return nil, err
		}
		result = append(result, revision)
	}
	return result, rows.Err()
}

func listExecutionGateResults(ctx context.Context, tx *sql.Tx, id string) ([]ExecutionGateResult, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, execution_id, gate, result, base_sha, target_sha, findings, created_at
		FROM gate_results WHERE execution_id = ? ORDER BY gate, created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ExecutionGateResult{}
	for rows.Next() {
		var gate ExecutionGateResult
		var findings string
		if err := rows.Scan(&gate.ID, &gate.ExecutionID, &gate.Gate, &gate.Result,
			&gate.BaseSHA, &gate.TargetSHA, &findings, &gate.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(findings), &gate.Findings); err != nil || gate.Findings == nil {
			return nil, fmt.Errorf("store: gate result %q has invalid findings", gate.ID)
		}
		result = append(result, gate)
	}
	return result, rows.Err()
}
