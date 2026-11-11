package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func decisionError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return &Error{Code: NotFound, Message: "Decision not found"}
	}
	return approvalError(err)
}

func listDecisions(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			WriteError(w, logger, invalid("query", "invalid"))
			return
		}
		status, kind := "open", ""
		if values, ok := query["status"]; ok {
			if len(values) != 1 || !slices.Contains([]string{"open", "answered", "withdrawn"}, values[0]) {
				WriteError(w, logger, invalid("status", "invalid"))
				return
			}
			status = values[0]
		}
		if values, ok := query["kind"]; ok {
			if len(values) != 1 || model.DecisionKind(values[0]).Validate() != nil {
				WriteError(w, logger, invalid("kind", "invalid"))
				return
			}
			kind = values[0]
		}
		resource := "decisions/" + status + "/" + kind
		page, err := ParsePage(query, resource)
		if err == nil && page.After != "" {
			err = ValidateID("cursor", page.After, ULID)
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		rows, err := store.ListOwnerDecisions(r.Context(), db, status, kind, page.After, page.Limit+1)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, NewPage(rows, page.Limit, resource, func(d store.OwnerDecision) string { return d.ID }))
	}
}

type decisionDetailView struct {
	store.OwnerDecision
	Approval      *approvalView   `json:"approval,omitempty"`
	RouteProposal json.RawMessage `json:"routeProposal,omitempty"`
}

func getDecision(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		defer tx.Rollback()
		decision, err := store.GetOwnerDecision(r.Context(), tx, id)
		view := decisionDetailView{OwnerDecision: decision}
		if err == nil && decision.Kind == "approval" && decision.SubjectID != nil {
			var approval store.Approval
			approval, err = store.GetApprovalTx(r.Context(), tx, *decision.SubjectID)
			if err == nil {
				var base approvalView
				base, err = newApprovalView(approval)
				view.Approval = &base
			}
		}
		if err == nil && decision.Kind == "route" && decision.SubjectID != nil {
			view.RouteProposal, err = store.GetRouteProposalJSON(r.Context(), tx, *decision.SubjectID)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			WriteError(w, logger, decisionError(err))
			return
		}
		writeJSON(w, http.StatusOK, view)
	}
}

type decisionAnswer struct {
	Action    string  `json:"action"`
	Label     string  `json:"label,omitempty"`
	Basis     string  `json:"basis,omitempty"`
	ContentID string  `json:"contentId,omitempty"`
	TTLSec    *int64  `json:"ttlSec,omitempty"`
	Text      string  `json:"text,omitempty"`
	Option    *string `json:"option,omitempty"`
}

// Validation follows each individual answer shape in 02 5.6, including
// forbidden fields whose value happens to be null or an empty string.
func validateDecisionAnswer(decision store.OwnerDecision, fields map[string]json.RawMessage) (decisionAnswer, error) {
	var answer decisionAnswer
	encoded, err := json.Marshal(fields)
	if err != nil {
		return answer, err
	}
	if err = json.Unmarshal(encoded, &answer); err != nil {
		return answer, invalid("body", "invalid_json")
	}
	required := []string{"action"}
	allowed := []string{"action"}
	valid := false
	switch decision.Kind {
	case "route":
		valid = answer.Action == "approve" || answer.Action == "reject"
		if answer.Action == "approve" {
			required = append(required, "label", "basis")
			allowed = append(allowed, "label", "basis")
		}
	case "approval":
		valid = answer.Action == "approve" || answer.Action == "reject"
		required = append(required, "contentId")
		allowed = append(allowed, "contentId")
		if answer.Action == "approve" {
			allowed = append(allowed, "ttlSec")
		}
	case "blocked":
		if decision.Cause == nil {
			return answer, fmt.Errorf("decision %s has no cause", decision.ID)
		}
		switch *decision.Cause {
		case "check_blocked":
			valid = answer.Action == "continue" || answer.Action == "abort"
			required = append(required, "contentId")
			allowed = append(allowed, "contentId")
		case "publish_unknown":
			valid = answer.Action == "continue" || answer.Action == "abort"
		case "context_conflict", "workspace_missing":
			valid = answer.Action == "abort"
		}
	case "question":
		valid = answer.Action == "answer" || answer.Action == "abort"
		if answer.Action == "answer" {
			required = append(required, "text", "option")
			allowed = append(allowed, "text", "option")
		}
	}
	if !valid {
		return answer, invalid("action", "invalid")
	}
	for _, key := range required {
		value, ok := fields[key]
		if !ok || (key != "option" && bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			return answer, invalid(key, "required")
		}
	}
	for key, value := range fields {
		if !slices.Contains(allowed, key) {
			return answer, invalid(key, "not_allowed")
		}
		if key == "ttlSec" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return answer, invalid(key, "invalid")
		}
	}
	if slices.Contains(required, "contentId") {
		if len(answer.ContentID) < 1 || len(answer.ContentID) > 256 {
			return answer, invalid("contentId", "invalid_format")
		}
		for _, char := range answer.ContentID {
			if char < 32 || char > 126 {
				return answer, invalid("contentId", "invalid_format")
			}
		}
	}
	if slices.Contains(required, "label") && answer.Label == "" {
		return answer, invalid("label", "required")
	}
	if slices.Contains(required, "basis") && answer.Basis == "" {
		return answer, invalid("basis", "required")
	}
	if answer.TTLSec != nil && *answer.TTLSec < 60 {
		return answer, invalid("ttlSec", "out_of_range")
	}
	return answer, nil
}

// The API owns shape/basis checks and the answer/audit write. The lifecycle
// implementation owns the corresponding domain changes in the same transaction.
type decisionAnswerApplier interface {
	ApplyAnswer(context.Context, *sql.Tx, store.OwnerDecision, decisionAnswer, time.Time) error
}

func answerDecision(db *sql.DB, logger *slog.Logger, applier decisionAnswerApplier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		var fields map[string]json.RawMessage
		if err := DecodeJSON(r, &fields); err != nil {
			WriteError(w, logger, err)
			return
		}
		decision, err := saveDecisionAnswer(r.Context(), db, id, fields, applier)
		if err != nil {
			WriteError(w, logger, decisionError(err))
			return
		}
		writeJSON(w, http.StatusOK, decision)
	}
}

func saveDecisionAnswer(ctx context.Context, db *sql.DB, id string, fields map[string]json.RawMessage, applier decisionAnswerApplier) (store.OwnerDecision, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return store.OwnerDecision{}, err
	}
	defer tx.Rollback()
	decision, err := store.GetOwnerDecision(ctx, tx, id)
	if err != nil {
		return decision, err
	}
	if decision.Status != "open" {
		return decision, &Error{Code: Conflict, Message: "Decision is already answered or withdrawn"}
	}
	answer, err := validateDecisionAnswer(decision, fields)
	if err != nil {
		return decision, err
	}
	basis := ""
	if decision.Kind == "approval" || (decision.Cause != nil && *decision.Cause == "check_blocked") {
		basis = answer.ContentID
	}
	if decision.Kind == "route" && answer.Action == "approve" {
		basis = answer.Basis
	}
	if basis != "" && (decision.Basis == nil || basis != *decision.Basis) {
		return decision, &Error{Code: PreconditionFailed, Message: "Answer basis does not match the decision"}
	}
	if decision.Kind == "route" && answer.Action == "approve" {
		if _, err := store.GetRoute(ctx, tx, answer.Label); errors.Is(err, sql.ErrNoRows) {
			return decision, &Error{Code: PreconditionFailed, Message: "Route label is no longer registered"}
		} else if err != nil {
			return decision, err
		}
	}
	nowTime := time.Now().UTC()
	if err := applier.ApplyAnswer(ctx, tx, decision, answer, nowTime); err != nil {
		return decision, err
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return decision, err
	}
	now := nowTime.Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE owner_decisions SET status='answered',answer=?,answered_at=? WHERE id=? AND status='open'`, string(encoded), now, id)
	if err != nil {
		return decision, err
	}
	if count, err := result.RowsAffected(); err != nil {
		return decision, err
	} else if count != 1 {
		return decision, &Error{Code: Conflict, Message: "Decision changed during answer"}
	}
	if _, err := store.RecordAuditLog(ctx, tx, store.AuditLog{Actor: "owner", Action: "decision.answer", TargetType: "owner_decision", TargetID: id, Detail: map[string]any{"answer": json.RawMessage(encoded)}}); err != nil {
		return decision, err
	}
	if decision.ExecutionID != nil {
		payload, _ := json.Marshal(map[string]string{"decisionId": id, "kind": decision.Kind, "action": answer.Action})
		if _, err := store.AppendExecutionEvent(ctx, tx, *decision.ExecutionID, model.EventDecisionAnswered, payload); err != nil {
			return decision, err
		}
	}
	decision, err = store.GetOwnerDecision(ctx, tx, id)
	if err != nil {
		return decision, err
	}
	return decision, tx.Commit()
}

// lifecycleAnswers applies spec 04 3.6 for approval, blocked and question.
// The route decision belongs to 3.7 and is not connected here.
type lifecycleAnswers struct{}

func (lifecycleAnswers) ApplyAnswer(ctx context.Context, tx *sql.Tx, decision store.OwnerDecision, answer decisionAnswer, now time.Time) error {
	switch decision.Kind {
	case "blocked", "question":
		return store.ApplyBlockedOrQuestionAnswerTx(ctx, tx, decision, answer.Action)
	case "approval":
	default:
		return fmt.Errorf("decision lifecycle handler is not connected for kind %q", decision.Kind)
	}
	if decision.SubjectID == nil {
		return fmt.Errorf("approval decision %s has no approval", decision.ID)
	}
	approval, err := store.GetApprovalTx(ctx, tx, *decision.SubjectID)
	if err != nil {
		return err
	}
	if decision.ExecutionID == nil || approval.DecisionID != decision.ID || approval.TaskID != decision.TaskID || approval.ExecutionID != *decision.ExecutionID {
		return fmt.Errorf("approval does not belong to decision %s", decision.ID)
	}
	return saveApprovalDecisionTx(ctx, tx, *decision.SubjectID, approvalDecisionInput{Decision: answer.Action, ContentVersion: answer.ContentID, TTLSec: answer.TTLSec}, now)
}
