package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

type ExecutionDetail struct {
	TaskExecution
	TaskID                               string
	RunnerID, Role, HarnessID, ProfileID *string
	ContextRevisions                     []ExecutionContextRevision
	Params                               json.RawMessage
	ContractVersion, ResumeStage         *string
	StageRuns                            []ExecutionStageRun
	CheckResults                         []CheckResult
	Artifacts                            []ExecutionArtifact
}

type ExecutionStageRun struct {
	ID, Stage, Status string
	Attempt           int
	ErrorClass        *string
	StartedAt         string
	FinishedAt        *string
	ResultArtifactID  *string
}

// ExecutionArtifact includes only metadata needed to list and link files.
type ExecutionArtifact struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Stage string `json:"stage"`
	Name  string `json:"name"`
}

// ExecutionContextRevision deliberately excludes the pinned document body.
type ExecutionContextRevision struct {
	ID, Layer, Kind, SourcePath, SourceCommitSHA, ContentHash string
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
	var params, outputs string
	err = tx.QueryRowContext(ctx, `SELECT id, task_id, seq, mode, status, error_class,
		profile_version, profile_commit_sha, started_at, finished_at,
		runner_id, NULL, harness_id, profile_id, coalesce(params, 'null'),
		contract_version, resume_stage, coalesce(outputs, '[]') FROM executions WHERE id = ?`, id).Scan(
		&result.ID, &result.TaskID, &result.Seq, &result.Mode, &result.Status, &result.ErrorClass,
		&result.ProfileVersion, &result.ProfileCommitSHA, &result.StartedAt, &result.FinishedAt,
		&result.RunnerID, &result.Role, &result.HarnessID, &result.ProfileID,
		&params, &result.ContractVersion, &result.ResumeStage, &outputs)
	if err != nil {
		return result, err
	}
	result.Params, result.Outputs = json.RawMessage(params), json.RawMessage(outputs)
	result.ContextRevisions, err = listExecutionContextMetadata(ctx, tx, id)
	if err != nil {
		return result, err
	}
	result.StageRuns, err = listExecutionStageRuns(ctx, tx, id)
	if err != nil {
		return result, err
	}
	result.CheckResults, err = listExecutionCheckResults(ctx, tx, id)
	if err != nil {
		return result, err
	}
	result.Artifacts, err = listExecutionArtifacts(ctx, tx, id)
	if err == nil {
		err = tx.Commit()
	}
	return result, err
}

func listExecutionStageRuns(ctx context.Context, tx *sql.Tx, id string) ([]ExecutionStageRun, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, stage, attempt, status, error_class, started_at, finished_at, result_artifact_id
		FROM stage_runs WHERE execution_id = ? ORDER BY started_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ExecutionStageRun{}
	for rows.Next() {
		var run ExecutionStageRun
		if err := rows.Scan(&run.ID, &run.Stage, &run.Attempt, &run.Status, &run.ErrorClass,
			&run.StartedAt, &run.FinishedAt, &run.ResultArtifactID); err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, rows.Err()
}

func listExecutionCheckResults(ctx context.Context, tx *sql.Tx, id string) ([]CheckResult, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, result, content_id, summary, findings, created_at
		FROM check_results WHERE execution_id = ? ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []CheckResult{}
	for rows.Next() {
		var check CheckResult
		var findings string
		if err := rows.Scan(&check.ID, &check.Result, &check.ContentID, &check.Summary, &findings, &check.CreatedAt); err != nil {
			return nil, err
		}
		check.Findings = json.RawMessage(findings)
		result = append(result, check)
	}
	return result, rows.Err()
}

func listExecutionArtifacts(ctx context.Context, tx *sql.Tx, id string) ([]ExecutionArtifact, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, stage, name FROM artifacts
		WHERE execution_id = ? ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ExecutionArtifact{}
	for rows.Next() {
		var artifact ExecutionArtifact
		if err := rows.Scan(&artifact.ID, &artifact.Kind, &artifact.Stage, &artifact.Name); err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	return result, rows.Err()
}

func listExecutionContextMetadata(ctx context.Context, tx *sql.Tx, id string) ([]ExecutionContextRevision, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, layer, kind, source_path, source_revision, content_hash
		FROM context_revisions WHERE execution_id = ?
		ORDER BY CASE layer WHEN 'global' THEN 0 WHEN 'harness' THEN 1 WHEN 'reported' THEN 2 ELSE 3 END,
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
