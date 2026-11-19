package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// Spec 13 3.4 / 01 5.1: the kind=approval decision detail carries the
// Execution's check result (summary and findings) read from check_results.
func TestApprovalDecisionDetailCarriesCheckResult(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	approvalDeclarations(t, f, id)
	attempt := runTo(t, f, token, id, "check")
	finishStage(t, f, token, id, finished("check", attempt, programResult("check", "needs_approval", map[string]any{
		"summary": "Two files changed",
		"findings": []any{map[string]any{"rule": "no_secrets", "severity": "warning", "message": "Looks like a key",
			"location": map[string]any{"path": "a.go", "line": 3}}},
		"approvalRequests": []any{stageApprovalRequest("merge_pull_request")},
	})))
	requireState(t, f, id, "waiting_approval", "waiting_approval", "")
	decision := openDecisionID(t, f, id, "approval")
	status, body := approvalRequest(t, f, "GET", "/api/decisions/"+decision, "")
	var detail struct {
		CheckResult *struct {
			Result    string  `json:"result"`
			ContentID *string `json:"contentId"`
			Summary   *string `json:"summary"`
			Findings  []struct {
				Rule, Severity, Message string
				Location                *struct {
					Path string
					Line int
				}
			} `json:"findings"`
		} `json:"checkResult"`
	}
	if status != 200 || json.Unmarshal(body, &detail) != nil || detail.CheckResult == nil {
		t.Fatalf("detail=%d %s", status, body)
	}
	c := detail.CheckResult
	if c.Result != "pass" || c.ContentID == nil || *c.ContentID != "content-1" || c.Summary == nil || *c.Summary != "Two files changed" ||
		len(c.Findings) != 1 || c.Findings[0].Rule != "no_secrets" || c.Findings[0].Severity != "warning" ||
		c.Findings[0].Message != "Looks like a key" || c.Findings[0].Location == nil || c.Findings[0].Location.Line != 3 {
		t.Fatalf("checkResult=%s", body)
	}

	// A newer check row of another content does not replace the one whose
	// content_id matches the approval's.
	stageExec(t, f, `INSERT INTO check_results (id, execution_id, stage_run_id, result, content_id, summary, findings, created_at)
		SELECT '01JZZZZZZZZZZZZZZZZZZZZZZZ', execution_id, stage_run_id, 'fail', 'other-content', 'Other', '[]', '2999-01-01T00:00:00Z'
		FROM check_results WHERE execution_id = ?`, id)
	status, body = approvalRequest(t, f, "GET", "/api/decisions/"+decision, "")
	if status != 200 || !strings.Contains(string(body), `"summary":"Two files changed"`) {
		t.Fatalf("matching content not preferred: %d %s", status, body)
	}

	// Without any check result the field is omitted.
	stageExec(t, f, `DELETE FROM check_results WHERE execution_id = ?`, id)
	status, body = approvalRequest(t, f, "GET", "/api/decisions/"+decision, "")
	if status != 200 || strings.Contains(string(body), `"checkResult"`) {
		t.Fatalf("without check result=%d %s", status, body)
	}
}
