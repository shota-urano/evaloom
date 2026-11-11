package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

type OwnerDecision struct {
	ID              string          `json:"id"`
	Kind            string          `json:"kind"`
	TaskID          string          `json:"taskId"`
	ExecutionID     *string         `json:"executionId"`
	SubjectID       *string         `json:"subjectId"`
	Cause           *string         `json:"cause"`
	Basis           *string         `json:"basis"`
	Detail          json.RawMessage `json:"detail"`
	Status          string          `json:"status"`
	WithdrawnReason *string         `json:"withdrawnReason"`
	Answer          json.RawMessage `json:"answer"`
	AnsweredAt      *string         `json:"answeredAt"`
	CreatedAt       string          `json:"createdAt"`
}

const decisionColumns = `id,kind,task_id,execution_id,subject_id,cause,basis,detail,status,withdrawn_reason,answer,answered_at,created_at`

func scanDecision(row rowScanner) (OwnerDecision, error) {
	var decision OwnerDecision
	var detail string
	var answer *string
	err := row.Scan(&decision.ID, &decision.Kind, &decision.TaskID, &decision.ExecutionID, &decision.SubjectID, &decision.Cause, &decision.Basis, &detail, &decision.Status, &decision.WithdrawnReason, &answer, &decision.AnsweredAt, &decision.CreatedAt)
	decision.Detail = json.RawMessage(detail)
	if answer != nil {
		decision.Answer = json.RawMessage(*answer)
	}
	return decision, err
}

func GetOwnerDecision(ctx context.Context, reader projectReader, id string) (OwnerDecision, error) {
	return scanDecision(reader.QueryRowContext(ctx, `SELECT `+decisionColumns+` FROM owner_decisions WHERE id=?`, id))
}

func ListOwnerDecisions(ctx context.Context, db *sql.DB, status, kind, before string, limit int) ([]OwnerDecision, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+decisionColumns+` FROM owner_decisions
		WHERE status=? AND (?='' OR kind=?) AND (?='' OR id<?) ORDER BY id DESC LIMIT ?`, status, kind, kind, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OwnerDecision{}
	for rows.Next() {
		decision, err := scanDecision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, decision)
	}
	return out, rows.Err()
}

// GetRouteProposalJSON returns the proposal's table fields using API names.
func GetRouteProposalJSON(ctx context.Context, reader projectReader, id string) (json.RawMessage, error) {
	var value string
	err := reader.QueryRowContext(ctx, `SELECT json_object('id',id,'taskId',task_id,'trigger',trigger,
		'seenLabels',json(seen_labels),'candidates',json(candidates),'status',status,'runnerId',runner_id,
		'proposedLabel',proposed_label,'reason',reason,'errorClass',error_class,'decisionId',decision_id,
		'createdAt',created_at,'finishedAt',finished_at) FROM route_proposals WHERE id=?`, id).Scan(&value)
	return json.RawMessage(value), err
}
