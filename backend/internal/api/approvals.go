package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// defaultApprovalTTL is the Q55 default validity of an approval: 24 hours,
// changeable by the owner per approval with ttlSec (spec 17 3.4, 5章).
const defaultApprovalTTL = 24 * time.Hour

// approvalRejectedReason is the Q57 failure reason of a rejected approval.
// executions.error_class has no value for it (spec 02 5.4), so it is kept
// in error_message and in the status_changed event reason.
const approvalRejectedReason = "approval_rejected"

var approvalStatuses = map[string]bool{
	"pending": true, "approved": true, "rejected": true, "consumed": true, "invalidated": true, "expired": true,
}

type approvalView struct {
	ID                string          `json:"id"`
	TaskID            string          `json:"taskId"`
	ExecutionID       string          `json:"executionId"`
	Operation         string          `json:"operation"`
	Target            json.RawMessage `json:"target"`
	ContentVersion    string          `json:"contentVersion"`
	ParamsHash        string          `json:"paramsHash"`
	Status            string          `json:"status"`
	InvalidatedReason *string         `json:"invalidatedReason"`
	DecidedBy         *string         `json:"decidedBy"`
	DecidedAt         *Timestamp      `json:"decidedAt"`
	ExpiresAt         *Timestamp      `json:"expiresAt"`
	MaxUses           int             `json:"maxUses"`
	UseCount          int             `json:"useCount"`
	ConsumedAt        *Timestamp      `json:"consumedAt"`
	Result            json.RawMessage `json:"result"`
	CreatedAt         Timestamp       `json:"createdAt"`
}

type approvalArtifactView struct {
	ID        string    `json:"id"`
	SHA256    string    `json:"sha256"`
	SizeBytes int64     `json:"sizeBytes"`
	CreatedAt Timestamp `json:"createdAt"`
}

type approvalDetailView struct {
	approvalView
	PRURL         *string                   `json:"prUrl"`
	GateResults   []executionGateResultView `json:"gateResults"`
	DiffArtifacts []approvalArtifactView    `json:"diffArtifacts"`
}

func newApprovalView(a store.Approval) (approvalView, error) {
	view := approvalView{
		ID: a.ID, TaskID: a.TaskID, ExecutionID: a.ExecutionID, Operation: a.Operation, Target: a.Target,
		ContentVersion: a.ContentVersion, ParamsHash: a.ParamsHash, Status: a.Status,
		InvalidatedReason: a.InvalidatedReason, DecidedBy: a.DecidedBy, MaxUses: a.MaxUses, UseCount: a.UseCount,
		Result: a.Result,
	}
	if view.Result == nil {
		view.Result = json.RawMessage("null")
	}
	created, err := ParseTimestamp(a.CreatedAt)
	if err != nil {
		return view, fmt.Errorf("approval %q: invalid stored creation time", a.ID)
	}
	view.CreatedAt = created
	for _, field := range []struct {
		stored *string
		target **Timestamp
	}{{a.DecidedAt, &view.DecidedAt}, {a.ExpiresAt, &view.ExpiresAt}, {a.ConsumedAt, &view.ConsumedAt}} {
		if field.stored == nil {
			continue
		}
		value, err := ParseTimestamp(*field.stored)
		if err != nil {
			return view, fmt.Errorf("approval %q: invalid stored timestamp", a.ID)
		}
		*field.target = &value
	}
	return view, nil
}

func listApprovals(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			WriteError(w, logger, invalid("query", "invalid"))
			return
		}
		page, err := ParsePage(query, "approvals")
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		var status string
		if values, ok := query["status"]; ok && !(len(values) == 1 && values[0] == "") {
			if len(values) != 1 || !approvalStatuses[values[0]] {
				WriteError(w, logger, invalid("status", "invalid"))
				return
			}
			status = values[0]
		}
		if page.After != "" && ValidateID("cursor", page.After, ULID) != nil {
			WriteError(w, logger, invalid("cursor", "invalid"))
			return
		}
		rows, err := store.ListApprovals(r.Context(), db, status, page.After, page.Limit+1)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		views := make([]approvalView, 0, len(rows))
		for _, row := range rows {
			view, err := newApprovalView(row)
			if err != nil {
				WriteError(w, logger, err)
				return
			}
			views = append(views, view)
		}
		writeJSON(w, http.StatusOK, NewPage(views, page.Limit, "approvals", func(v approvalView) string { return v.ID }))
	}
}

func getApproval(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ValidateID("id", id, ULID); err != nil {
			WriteError(w, logger, err)
			return
		}
		view, err := approvalDetail(r.Context(), db, id)
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	}
}

func approvalDetail(ctx context.Context, db *sql.DB, id string) (approvalDetailView, error) {
	detail, err := store.GetApprovalDetail(ctx, db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return approvalDetailView{}, &Error{Code: NotFound, Message: "Approval not found"}
	}
	if err != nil {
		return approvalDetailView{}, err
	}
	base, err := newApprovalView(detail.Approval)
	if err != nil {
		return approvalDetailView{}, err
	}
	view := approvalDetailView{approvalView: base, PRURL: detail.PRURL,
		GateResults:   make([]executionGateResultView, 0, len(detail.GateResults)),
		DiffArtifacts: make([]approvalArtifactView, 0, len(detail.DiffArtifacts)),
	}
	for _, gate := range detail.GateResults {
		created, err := ParseTimestamp(gate.CreatedAt)
		if err != nil {
			return view, fmt.Errorf("gate result %q: invalid stored timestamp", gate.ID)
		}
		view.GateResults = append(view.GateResults, executionGateResultView{
			ID: gate.ID, Gate: gate.Gate, Result: gate.Result, BaseSHA: gate.BaseSHA,
			TargetSHA: gate.TargetSHA, Findings: gate.Findings, CreatedAt: created,
		})
	}
	for _, artifact := range detail.DiffArtifacts {
		created, err := ParseTimestamp(artifact.CreatedAt)
		if err != nil {
			return view, fmt.Errorf("artifact %q: invalid stored timestamp", artifact.ID)
		}
		view.DiffArtifacts = append(view.DiffArtifacts, approvalArtifactView{
			ID: artifact.ID, SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes, CreatedAt: created,
		})
	}
	return view, nil
}

type approvalDecisionInput struct {
	Decision       string `json:"decision"`
	ContentVersion string `json:"contentVersion"`
	TTLSec         *int64 `json:"ttlSec"`
}

// maxApprovalTTLSec keeps now + ttlSec representable as a time.Duration. It
// is a technical bound, not a policy value (Q55 leaves the range open).
const maxApprovalTTLSec = math.MaxInt64 / int64(time.Second)

func decideApproval(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := ValidateID("id", id, ULID)
		var input approvalDecisionInput
		if err == nil {
			err = DecodeJSON(r, &input)
		}
		if err == nil {
			switch input.Decision {
			case "approve", "reject":
			case "":
				err = invalid("decision", "required")
			default:
				err = invalid("decision", "invalid")
			}
		}
		if err == nil && input.ContentVersion == "" {
			err = invalid("contentVersion", "required")
		}
		if err == nil && !fullCommitSHA(input.ContentVersion) {
			err = invalid("contentVersion", "invalid_format")
		}
		if err == nil && input.TTLSec != nil {
			if input.Decision != "approve" {
				err = invalid("ttlSec", "not_allowed")
			} else if *input.TTLSec < 1 || *input.TTLSec > maxApprovalTTLSec {
				err = invalid("ttlSec", "out_of_range")
			}
		}
		if err == nil {
			err = saveApprovalDecision(r.Context(), db, id, input)
		}
		if err == nil {
			var view approvalDetailView
			view, err = approvalDetail(r.Context(), db, id)
			if err == nil {
				writeJSON(w, http.StatusOK, view)
				return
			}
		}
		WriteError(w, logger, approvalError(err))
	}
}

func approvalError(err error) error {
	if errors.Is(err, task.ErrConflict) || errors.Is(err, store.ErrApprovalStale) {
		return &Error{Code: Conflict, Message: err.Error()}
	}
	return err
}

// readApprovalForUpdate loads the approval and its Execution inside the
// caller's write transaction.
func readApprovalForUpdate(ctx context.Context, tx *sql.Tx, id string) (store.Approval, store.ContextExecution, error) {
	approval, err := store.GetApprovalTx(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return approval, store.ContextExecution{}, &Error{Code: NotFound, Message: "Approval not found"}
	}
	if err != nil {
		return approval, store.ContextExecution{}, err
	}
	execution, err := store.GetContextExecution(ctx, tx, approval.ExecutionID)
	return approval, execution, err
}

// saveApprovalDecision records approve or reject (spec 17 3.4, 3.5.1, 3.6)
// with its state transition, event and audit record in one transaction.
func saveApprovalDecision(ctx context.Context, db *sql.DB, id string, input approvalDecisionInput) error {
	tx, err := db.BeginTx(ctx, nil) // BEGIN IMMEDIATE serializes decisions with consume, cancel and results.
	if err != nil {
		return err
	}
	defer tx.Rollback()
	approval, execution, err := readApprovalForUpdate(ctx, tx, id)
	if err != nil {
		return err
	}
	if approval.Status != "pending" {
		return &Error{Code: Conflict, Message: "Approval is already decided, invalidated or expired"}
	}
	if !execution.Latest || execution.Status != model.ExecutionWaitingApproval || execution.TaskStatus != model.TaskWaitingApproval {
		return &Error{Code: Conflict, Message: "Decision requires the latest Execution waiting for approval"}
	}
	if input.ContentVersion != approval.ContentVersion {
		return &Error{Code: PreconditionFailed, Message: "contentVersion must equal the approval's content version"}
	}
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	status, to, reason := "approved", model.ExecutionMerging, "approval_approved"
	var expiresAt *string
	if input.Decision == "approve" {
		ttl := defaultApprovalTTL
		if input.TTLSec != nil {
			ttl = time.Duration(*input.TTLSec) * time.Second
		}
		value := now.Add(ttl).Format(time.RFC3339Nano)
		expiresAt = &value
	} else {
		status, to, reason = "rejected", model.ExecutionFailed, approvalRejectedReason
	}
	if err := store.DecideApprovalTx(ctx, tx, id, input.ContentVersion, status, expiresAt, stamp); err != nil {
		return err
	}
	if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
		TaskID: execution.TaskID, ExecutionID: execution.ID, FromTask: model.TaskWaitingApproval,
		FromExecution: model.ExecutionWaitingApproval, ToExecution: to,
	}); err != nil {
		return err
	}
	if to == model.ExecutionFailed {
		// Q57: the PR stays open; the owner fixes it with a continue retry.
		if _, err := tx.ExecContext(ctx, `UPDATE executions SET error_message = ? WHERE id = ?`,
			approvalRejectedReason, execution.ID); err != nil {
			return err
		}
	}
	payload, _ := json.Marshal(map[string]string{"from": string(model.ExecutionWaitingApproval), "to": string(to), "reason": reason})
	if _, err := store.AppendExecutionEvent(ctx, tx, execution.ID, model.EventStatusChanged, payload); err != nil {
		return err
	}
	detail := map[string]any{"taskId": approval.TaskID, "executionId": approval.ExecutionID, "operation": approval.Operation,
		"decision": input.Decision, "contentVersion": approval.ContentVersion}
	if expiresAt != nil {
		detail["expiresAt"] = *expiresAt
	}
	if _, err := store.RecordAuditLog(ctx, tx, store.AuditLog{
		Actor: "owner", Action: "approval.decide", TargetType: "approval", TargetID: id, Detail: detail,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func renewApproval(db *sql.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := ValidateID("id", id, ULID)
		var renewed string
		if err == nil {
			renewed, err = saveApprovalRenewal(r.Context(), db, id)
		}
		if err == nil {
			var view approvalDetailView
			view, err = approvalDetail(r.Context(), db, renewed)
			if err == nil {
				writeJSON(w, http.StatusCreated, view)
				return
			}
		}
		WriteError(w, logger, approvalError(err))
	}
}

// saveApprovalRenewal creates a new pending approval from an expired one
// (spec 17 3.6). An approved approval past expires_at whose merge has not
// consumed it is marked expired here and its Execution returns from merging
// to waiting_approval, as a refused consume does (3.5.2).
func saveApprovalRenewal(ctx context.Context, db *sql.DB, id string) (string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	approval, execution, err := readApprovalForUpdate(ctx, tx, id)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if !store.ApprovalExpired(approval, now) || approval.UseCount != 0 {
		return "", &Error{Code: Conflict, Message: "Only an expired approval can be renewed"}
	}
	from := execution.Status
	if !execution.Latest || execution.TaskStatus != model.TaskWaitingApproval ||
		(from != model.ExecutionWaitingApproval && from != model.ExecutionMerging) {
		return "", &Error{Code: Conflict, Message: "Renewal requires the latest Execution waiting for approval"}
	}
	var open int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM approvals WHERE execution_id = ? AND operation = ?
		AND status IN ('pending', 'approved') AND id <> ?`, approval.ExecutionID, approval.Operation, approval.ID).Scan(&open); err != nil {
		return "", err
	}
	if open != 0 {
		return "", &Error{Code: Conflict, Message: "The Execution already has an unused approval"}
	}
	stamp := now.Format(time.RFC3339Nano)
	renewed, err := store.RenewApprovalTx(ctx, tx, approval, stamp)
	if err != nil {
		return "", err
	}
	if from == model.ExecutionMerging {
		if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
			TaskID: execution.TaskID, ExecutionID: execution.ID, FromTask: model.TaskWaitingApproval,
			FromExecution: model.ExecutionMerging, ToExecution: model.ExecutionWaitingApproval,
		}); err != nil {
			return "", err
		}
		payload, _ := json.Marshal(map[string]string{"from": string(model.ExecutionMerging),
			"to": string(model.ExecutionWaitingApproval), "reason": "approval_expired"})
		if _, err := store.AppendExecutionEvent(ctx, tx, execution.ID, model.EventStatusChanged, payload); err != nil {
			return "", err
		}
	}
	if _, err := store.RecordAuditLog(ctx, tx, store.AuditLog{
		Actor: "owner", Action: "approval.renew", TargetType: "approval", TargetID: id,
		Detail: map[string]any{"taskId": approval.TaskID, "executionId": approval.ExecutionID, "operation": approval.Operation,
			"previousStatus": approval.Status, "renewedApprovalId": renewed, "contentVersion": approval.ContentVersion},
	}); err != nil {
		return "", err
	}
	return renewed, tx.Commit()
}
