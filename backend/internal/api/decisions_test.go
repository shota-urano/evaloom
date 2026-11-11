package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

type answerApplierFunc func(context.Context, *sql.Tx, store.OwnerDecision, decisionAnswer, time.Time) error

func (f answerApplierFunc) ApplyAnswer(ctx context.Context, tx *sql.Tx, d store.OwnerDecision, a decisionAnswer, now time.Time) error {
	return f(ctx, tx, d, a, now)
}

func seedDecision(t *testing.T, f *authFixture, executionID, kind, cause string) string {
	t.Helper()
	id := ulid.Make().String()
	var execution, subject, causeValue any
	if kind != "route" {
		execution = executionID
	}
	if kind == "route" {
		subject = "proposal"
	}
	if cause != "" {
		causeValue = cause
	}
	stageExec(t, f, `INSERT INTO owner_decisions (id,kind,task_id,execution_id,subject_id,cause,basis,detail,status,created_at)
 SELECT ?,?,task_id,?,?,?,'opaque-content','{"question":"Which?","options":["one","two"]}','open','2026-11-08T00:00:00Z' FROM executions WHERE id=?`, id, kind, execution, subject, causeValue, executionID)
	return id
}

func TestDecisionAnswerShapesAcrossAllKinds(t *testing.T) {
	for _, tc := range []struct{ kind, cause, valid string }{
		{"route", "", `{"action":"approve","label":"route:a","basis":"proposal"}`},
		{"route", "", `{"action":"reject"}`},
		{"approval", "", `{"action":"approve","contentId":"opaque-content","ttlSec":60}`},
		{"approval", "", `{"action":"reject","contentId":"opaque-content"}`},
		{"blocked", "check_blocked", `{"action":"continue","contentId":"opaque-content"}`},
		{"blocked", "check_blocked", `{"action":"abort","contentId":"opaque-content"}`},
		{"blocked", "publish_unknown", `{"action":"continue"}`},
		{"blocked", "publish_unknown", `{"action":"abort"}`},
		{"blocked", "context_conflict", `{"action":"abort"}`},
		{"blocked", "workspace_missing", `{"action":"abort"}`},
		{"question", "question", `{"action":"answer","text":"free text","option":null}`},
		{"question", "question", `{"action":"answer","text":"","option":"one"}`},
		{"question", "question", `{"action":"abort"}`},
	} {
		t.Run(tc.kind+"/"+tc.cause+"/"+tc.valid, func(t *testing.T) {
			d := store.OwnerDecision{Kind: tc.kind}
			if tc.cause != "" {
				d.Cause = &tc.cause
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.valid), &fields); err != nil {
				t.Fatal(err)
			}
			if _, err := validateDecisionAnswer(d, fields); err != nil {
				t.Fatalf("valid answer rejected: %v", err)
			}
			for _, key := range []string{"unknown", "Action", "kind", "cause", "decision", "contentVersion"} {
				fields[key] = json.RawMessage(`null`)
				if _, err := validateDecisionAnswer(d, fields); err == nil {
					t.Fatalf("accepted unknown %s", key)
				}
				delete(fields, key)
			}
			for _, body := range []string{`{}`, `{"action":"maybe"}`, `{"action":null}`, `{"action":1}`} {
				var invalidFields map[string]json.RawMessage
				json.Unmarshal([]byte(body), &invalidFields)
				if _, err := validateDecisionAnswer(d, invalidFields); err == nil {
					t.Fatalf("accepted %s", body)
				}
			}
		})
	}
}

func TestDecisionsRejectWrongShapeAndStaleBasisWithoutWrites(t *testing.T) {
	f, _, executionID, _, approval := waitingApprovalFixture(t)
	var approvalDecision string
	if err := f.db.QueryRow(`SELECT decision_id FROM approvals WHERE id=?`, approval.ID).Scan(&approvalDecision); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"action":"approve"}`, `{"action":"approve","contentId":null}`,
		`{"action":"approve","contentId":"opaque","ttlSec":59}`,
		`{"action":"approve","contentId":"opaque","ttlSec":60.0}`,
		`{"action":"approve","contentId":"opaque","ttlSec":null}`,
		`{"action":"reject","contentId":"opaque","ttlSec":null}`,
		`{"action":"approve","contentId":"opaque","basis":null}`,
		`{"decision":"approve","contentVersion":"opaque"}`,
		`[]`, `null`, `{} {}`,
	} {
		status, body := approvalRequest(t, f, "POST", "/api/decisions/"+approvalDecision+"/answer", body)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	for _, tc := range []struct{ kind, cause, body string }{
		{"approval", "", `{"action":"approve","contentId":"different"}`},
		{"route", "", `{"action":"approve","label":"route:a","basis":"different"}`},
		{"blocked", "check_blocked", `{"action":"continue","contentId":"different"}`},
		{"blocked", "check_blocked", `{"action":"abort","contentId":"different"}`},
	} {
		id := approvalDecision
		if tc.kind != "approval" {
			id = seedDecision(t, f, executionID, tc.kind, tc.cause)
		}
		status, body := approvalRequest(t, f, "POST", "/api/decisions/"+id+"/answer", tc.body)
		assertAuthError(t, status, body, 422, PreconditionFailed)
		if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE id=? AND status='open' AND answer IS NULL`, id); n != 1 {
			t.Fatal("rejected answer changed row")
		}
		if tc.kind != "approval" {
			stageExec(t, f, `UPDATE owner_decisions SET status='withdrawn',withdrawn_reason='superseded' WHERE id=?`, id)
		}
	}
	if n := stageCount(t, f, `SELECT count(*) FROM audit_logs WHERE action='decision.answer'`); n != 0 {
		t.Fatal("rejected answer was audited as answered")
	}
}

func TestDecisionAnswerUsesSameTransactionForDomainAndAnswer(t *testing.T) {
	f, _, executionID, _ := resultFixture(t, "working")
	id := seedDecision(t, f, executionID, "question", "question")
	handler := func(apply answerApplierFunc) *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", "/api/decisions/"+id+"/answer", strings.NewReader(`{"action":"answer","text":"owner reply","option":null}`))
		request.SetPathValue("id", id)
		response := httptest.NewRecorder()
		answerDecision(f.db, nil, apply)(response, request)
		return response
	}
	calls := 0
	apply := answerApplierFunc(func(ctx context.Context, tx *sql.Tx, d store.OwnerDecision, a decisionAnswer, now time.Time) error {
		calls++
		if a.Action != "answer" || a.Text != "owner reply" || d.ID != id {
			t.Fatal("wrong dispatch")
		}
		_, err := tx.ExecContext(ctx, `UPDATE executions SET error_message='domain change' WHERE id=?`, executionID)
		return err
	})
	stageExec(t, f, `CREATE TRIGGER fail_decision_audit BEFORE INSERT ON audit_logs WHEN NEW.action='decision.answer' BEGIN SELECT RAISE(ABORT,'audit failed'); END`)
	if response := handler(apply); response.Code != 500 {
		t.Fatalf("failed audit=%d %s", response.Code, response.Body)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM executions WHERE id=? AND error_message='domain change'`, executionID); n != 0 {
		t.Fatal("domain write escaped rollback")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE id=? AND status='open' AND answer IS NULL`, id); n != 1 {
		t.Fatal("answer escaped rollback")
	}
	stageExec(t, f, `DROP TRIGGER fail_decision_audit`)
	if response := handler(apply); response.Code != 200 {
		t.Fatalf("answer=%d %s", response.Code, response.Body)
	}
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE id=? AND status='answered' AND json_extract(answer,'$.text')='owner reply'`, id); n != 1 {
		t.Fatal("answer not saved")
	}
	if n := stageCount(t, f, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='decision_answered' AND json_extract(payload,'$.decisionId')=?`, executionID, id); n != 1 {
		t.Fatal("event not saved")
	}
	if response := handler(apply); response.Code != 409 {
		t.Fatalf("second answer=%d %s", response.Code, response.Body)
	}
	if calls != 2 {
		t.Fatal("answered decision dispatched twice")
	}
}

func TestDecisionListDetailAndUnconnectedLifecycle(t *testing.T) {
	f, _, executionID, _, approval := waitingApprovalFixture(t)
	var decisionID string
	if err := f.db.QueryRow(`SELECT decision_id FROM approvals WHERE id=?`, approval.ID).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	questionID := seedDecision(t, f, executionID, "question", "question")
	status, body := approvalRequest(t, f, "GET", "/api/decisions?kind=approval", "")
	var page Page[store.OwnerDecision]
	if status != 200 || json.Unmarshal(body, &page) != nil || len(page.Items) != 1 || page.Items[0].ID != decisionID {
		t.Fatalf("filtered list=%d %s", status, body)
	}
	status, body = approvalRequest(t, f, "GET", "/api/decisions?limit=1", "")
	if status != 200 || json.Unmarshal(body, &page) != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("page=%d %s", status, body)
	}
	status, body = approvalRequest(t, f, "GET", "/api/decisions?limit=1&cursor="+page.NextCursor, "")
	if status != 200 || json.Unmarshal(body, &page) != nil || len(page.Items) != 1 || page.NextCursor != "" {
		t.Fatalf("next=%d %s", status, body)
	}
	status, body = approvalRequest(t, f, "GET", "/api/decisions/"+decisionID, "")
	var detail decisionDetailView
	if status != 200 || json.Unmarshal(body, &detail) != nil || detail.Approval == nil || detail.Approval.ContentVersion != approval.ContentVersion {
		t.Fatalf("detail=%d %s", status, body)
	}
	if strings.Contains(string(body), `"contentVersion"`) {
		t.Fatal("legacy name in response")
	}
	for _, query := range []string{"status=invalid", "kind=invalid", "kind=route&kind=question", "status=open&status=answered"} {
		status, body = approvalRequest(t, f, "GET", "/api/decisions?"+query, "")
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	status, body = approvalRequest(t, f, "POST", "/api/decisions/"+questionID+"/answer", `{"action":"answer","text":"reply","option":null}`)
	assertAuthError(t, status, body, 500, Internal)
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE id=? AND status='open' AND answer IS NULL`, questionID); n != 1 {
		t.Fatal("unconnected lifecycle accepted answer")
	}
	// Test a domain failure before any answer/audit write.
	var fields map[string]json.RawMessage
	json.Unmarshal([]byte(`{"action":"abort"}`), &fields)
	_, err := saveDecisionAnswer(context.Background(), f.db, questionID, fields, answerApplierFunc(func(context.Context, *sql.Tx, store.OwnerDecision, decisionAnswer, time.Time) error {
		return errors.New("domain failure")
	}))
	if err == nil {
		t.Fatal("domain error ignored")
	}
}

func TestApprovalAnswerAcceptsOpaqueContentAndConfiguredTTL(t *testing.T) {
	f, _, _, _, approval := waitingApprovalFixture(t)
	stageExec(t, f, `UPDATE approvals SET content_id='release:r1' WHERE id=?`, approval.ID)
	stageExec(t, f, `UPDATE owner_decisions SET basis='release:r1' WHERE subject_id=?`, approval.ID)
	stageExec(t, f, `INSERT INTO settings VALUES ('approval.ttlSec','600','t')`)
	var id string
	if err := f.db.QueryRow(`SELECT decision_id FROM approvals WHERE id=?`, approval.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	status, body := approvalRequest(t, f, "POST", "/api/decisions/"+id+"/answer", `{"action":"approve","contentId":"release:r1"}`)
	var answered store.OwnerDecision
	if status != 200 || json.Unmarshal(body, &answered) != nil || answered.ID != id || answered.Status != "answered" {
		t.Fatalf("answer=%d %s", status, body)
	}
	status, body = approvalRequest(t, f, "GET", "/api/approvals/"+approval.ID, "")
	view := decodeApproval(t, status, body, 200)
	if view.ContentVersion != "release:r1" || view.DecisionID != id || view.ExpiresAt == nil || view.ExpiresAt.Before(before.Add(600*time.Second)) {
		t.Fatalf("approval=%s", body)
	}
	for _, legacy := range []string{`"contentVersion"`, `"decidedBy"`, `"result"`} {
		if strings.Contains(string(body), legacy) {
			t.Fatalf("legacy response field %s", legacy)
		}
	}
}

func TestRouteDecisionDetailUsesCurrentLabelTable(t *testing.T) {
	f, _, executionID, _ := resultFixture(t, "working")
	id := seedDecision(t, f, executionID, "route", "")
	stageExec(t, f, `INSERT INTO route_proposals (id,task_id,trigger,seen_labels,candidates,status,proposed_label,reason,decision_id,created_at)
 SELECT 'proposal',task_id,'no_label','[]','[]','proposed','route:a','suggested',?,'2026-11-08T00:00:00Z' FROM executions WHERE id=?`, id, executionID)
	status, body := approvalRequest(t, f, "GET", "/api/decisions/"+id, "")
	var detail decisionDetailView
	if status != 200 || json.Unmarshal(body, &detail) != nil || !strings.Contains(string(detail.RouteProposal), `"proposedLabel":"route:a"`) {
		t.Fatalf("proposal detail=%d %s", status, body)
	}
	status, body = approvalRequest(t, f, "POST", "/api/decisions/"+id+"/answer", `{"action":"approve","label":"route:deleted","basis":"opaque-content"}`)
	assertAuthError(t, status, body, 422, PreconditionFailed)
	if n := stageCount(t, f, `SELECT count(*) FROM owner_decisions WHERE id=? AND status='open' AND answer IS NULL`, id); n != 1 {
		t.Fatal("missing label changed decision")
	}
}
