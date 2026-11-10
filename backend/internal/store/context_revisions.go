package store

import (
	"context"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// ContextExecution reads only saved Execution pins and its Task input, never the
// current project_harnesses assignment or a moving Profile tag.
type ContextExecution struct {
	ID, TaskID, RunnerID, HarnessID, ProfileID, ProfileVersion, ProfileCommitSHA string
	Status                                                                       model.ExecutionStatus
	TaskStatus                                                                   model.TaskStatus
	Title, Body, AcceptanceCriteria                                              string
	Latest                                                                       bool
}

func GetContextExecution(ctx context.Context, reader projectReader, id string) (ContextExecution, error) {
	var execution ContextExecution
	err := reader.QueryRowContext(ctx, `SELECT e.id, e.task_id, COALESCE(e.runner_id, ''),
		COALESCE(e.harness_id, ''), COALESCE(e.profile_id, ''),
		COALESCE(e.profile_version, ''), COALESCE(e.profile_commit_sha, ''), e.status, t.status,
		t.title, t.body, t.acceptance_criteria,
		e.seq = (SELECT max(seq) FROM executions WHERE task_id = e.task_id)
		FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, id).Scan(
		&execution.ID, &execution.TaskID, &execution.RunnerID,
		&execution.HarnessID, &execution.ProfileID, &execution.ProfileVersion, &execution.ProfileCommitSHA,
		&execution.Status, &execution.TaskStatus, &execution.Title, &execution.Body, &execution.AcceptanceCriteria, &execution.Latest)
	return execution, err
}
