package api

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

type resultWorktree struct {
	Action      string             `json:"action"`
	ID          string             `json:"id"`
	Path        string             `json:"path,omitempty"`
	BranchName  string             `json:"branchName,omitempty"`
	BaseSHA     string             `json:"baseSha,omitempty"`
	CopiedFiles []model.CopiedFile `json:"copiedFiles,omitempty"`
}

type resultAudit struct {
	Action string         `json:"action"`
	Detail map[string]any `json:"detail"`
}

func validateResultAudit(input resultInput) error {
	a := input.Audit
	if a == nil {
		return nil
	}
	if input.Status != "failed" {
		return invalid("audit", "only_for_failed")
	}
	fields := []string{}
	switch a.Action {
	case "worktree.path_rejected":
		if input.ErrorClass != model.ErrorConfig {
			return invalid("audit", "error_class_mismatch")
		}
		fields = []string{"path"}
	case "publish.remote_mismatch":
		if input.ErrorClass != model.ErrorPublishFailed {
			return invalid("audit", "error_class_mismatch")
		}
		fields = []string{"branch", "verifiedSha", "observedSha"}
	default:
		return invalid("audit.action", "invalid_value")
	}
	if len(a.Detail) != len(fields) {
		return invalid("audit.detail", "invalid_format")
	}
	for _, field := range fields {
		value, ok := a.Detail[field].(string)
		// Publisher reports an empty observedSha if the remote branch vanished.
		if !ok || field != "observedSha" && strings.TrimSpace(value) == "" {
			return invalid("audit.detail", "invalid_format")
		}
	}
	return nil
}

func hasStageResult(input resultInput) bool {
	return input.Worktree != nil || input.PRCreated != nil ||
		input.FromStatus == input.Status && input.Status == "publishing" &&
			(input.PushedSHA != nil || input.PRNumber != nil || input.PRURL != nil)
}

// Stage receipts live in the existing execution.result audit detail. Besides
// documenting the report, they distinguish a PR confirmation from creation and
// preserve restore-start idempotency after the copy stage has completed.
func saveStageResult(r *http.Request, tx *sql.Tx, e store.ContextExecution, input resultInput) error {
	expected := resultInput{FromStatus: input.FromStatus, Status: input.Status}
	var step string
	var report any
	switch {
	case input.Worktree != nil:
		expected.Worktree = input.Worktree
		step, report = "worktree."+input.Worktree.Action, input.Worktree
	case input.PushedSHA != nil:
		expected.PushedSHA, expected.BranchName = input.PushedSHA, input.BranchName
		step, report = "pushed", map[string]any{"pushedSha": input.PushedSHA, "branchName": input.BranchName}
	default:
		expected.PRNumber, expected.PRURL, expected.PRCreated = input.PRNumber, input.PRURL, input.PRCreated
		step, report = "pr", map[string]any{"prNumber": input.PRNumber, "prUrl": input.PRURL, "prCreated": input.PRCreated}
	}
	if !reflect.DeepEqual(input, expected) {
		return invalid("result", "stage_report_only")
	}
	wantStatus := "publishing"
	if input.Worktree != nil {
		wantStatus = "preparing"
	}
	if input.FromStatus != wantStatus || input.Status != wantStatus {
		return invalid("result", "stage_status_mismatch")
	}

	// Validate scope and immutable identities before consulting receipts. In
	// particular base/path/branch mismatches retain the specified 422 response.
	if w := input.Worktree; w != nil {
		if err := validateStageWorktree(r, tx, e, w); err != nil {
			return err
		}
	} else if step == "pushed" {
		if input.BranchName == nil || strings.TrimSpace(*input.BranchName) == "" || !fullCommitSHA(*input.PushedSHA) {
			return invalid("pushedSha", "sha_and_branch_required")
		}
		var verified, branch string
		if err := tx.QueryRowContext(r.Context(), `SELECT coalesce(verified_sha,''),coalesce(branch_name,'') FROM executions WHERE id=?`, e.ID).Scan(&verified, &branch); err != nil {
			return err
		}
		if verified != *input.PushedSHA || branch != *input.BranchName {
			return &Error{Code: PreconditionFailed, Message: "Push must match the saved verified SHA and branch"}
		}
	} else {
		if input.PRNumber == nil || *input.PRNumber < 1 || input.PRURL == nil || strings.TrimSpace(*input.PRURL) == "" || input.PRCreated == nil {
			return invalid("prNumber", "pr_number_url_and_created_required")
		}
		var pushed string
		if err := tx.QueryRowContext(r.Context(), `SELECT coalesce(pushed_sha,'') FROM executions WHERE id=?`, e.ID).Scan(&pushed); err != nil {
			return err
		}
		if pushed == "" {
			return &Error{Code: PreconditionFailed, Message: "PR report requires a saved pushed SHA"}
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	var saved string
	err = tx.QueryRowContext(r.Context(), `SELECT json_extract(detail,'$.report') FROM audit_logs
  WHERE action='execution.result' AND target_type='execution' AND target_id=? AND json_extract(detail,'$.step')=? ORDER BY id LIMIT 1`, e.ID, step).Scan(&saved)
	if err == nil {
		var previous, current any
		if err := json.Unmarshal([]byte(saved), &previous); err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, &current); err != nil {
			return err
		}
		if !reflect.DeepEqual(previous, current) {
			return &Error{Code: Conflict, Message: "Stage report is immutable"}
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if w := input.Worktree; w != nil {
		err = saveStageWorktree(r, tx, e, w)
	} else {
		err = saveResult(r, tx, e, input)
		if err == nil && step == "pushed" {
			err = appendStageEvent(r, tx, e.ID, model.EventPushed, map[string]any{"sha": input.PushedSHA, "branch": input.BranchName})
		} else if err == nil && *input.PRCreated {
			err = appendStageEvent(r, tx, e.ID, model.EventPRCreated, map[string]any{"prNumber": input.PRNumber, "prUrl": input.PRURL})
		}
	}
	if err != nil {
		return err
	}
	_, err = store.RecordAuditLog(r.Context(), tx, store.AuditLog{
		Actor: "runner:" + e.RunnerID, Action: "execution.result", TargetType: "execution", TargetID: e.ID,
		Detail: map[string]any{"taskId": e.TaskID, "from": input.FromStatus, "to": input.Status, "step": step, "report": json.RawMessage(encoded)},
	})
	return err
}

func validateStageWorktree(r *http.Request, tx *sql.Tx, e store.ContextExecution, w *resultWorktree) error {
	if err := ValidateID("worktree.id", w.ID, ULID); err != nil {
		return err
	}
	switch w.Action {
	case "restore_started":
		if w.Path != "" || w.BranchName != "" || w.BaseSHA != "" || w.CopiedFiles != nil {
			return invalid("worktree", "restore_start_id_only")
		}
	case "created", "restored":
		if !filepath.IsAbs(w.Path) || strings.TrimSpace(w.BranchName) == "" || !fullCommitSHA(w.BaseSHA) || w.CopiedFiles == nil {
			return invalid("worktree", "path_branch_sha_and_copies_required")
		}
		seen := map[string]bool{}
		for _, file := range w.CopiedFiles {
			digest, err := hex.DecodeString(file.SHA256)
			if err != nil || len(digest) != 32 || !filepath.IsLocal(file.Path) || file.Path == "." || filepath.Clean(file.Path) != file.Path || seen[file.Path] {
				return invalid("worktree.copiedFiles", "invalid_format")
			}
			seen[file.Path] = true
		}
	default:
		return invalid("worktree.action", "invalid_value")
	}
	if w.Action == "created" {
		if w.BaseSHA != e.BaseSHA {
			return &Error{Code: PreconditionFailed, Message: "Worktree base SHA must equal the saved Execution base SHA"}
		}
		return nil
	}
	var path, branch, base string
	err := tx.QueryRowContext(r.Context(), `SELECT path,branch_name,base_sha FROM worktrees WHERE id=?`, w.ID).Scan(&path, &branch, &base)
	if errors.Is(err, sql.ErrNoRows) {
		return &Error{Code: PreconditionFailed, Message: "Predecessor worktree is required"}
	}
	if err != nil {
		return err
	}
	own, err := ownsPredecessorWorktree(r, tx, e.ID, resultInput{WorktreeID: &w.ID, BaseSHA: &base, BranchName: &branch})
	if err != nil {
		return err
	}
	if !own || e.BaseSHA != "" && e.BaseSHA != base {
		return &Error{Code: PreconditionFailed, Message: "Worktree must belong to the immediately preceding Execution"}
	}
	if w.Action == "restored" && (w.Path != path || w.BranchName != branch || w.BaseSHA != base) {
		return &Error{Code: PreconditionFailed, Message: "Restored worktree identity must match the saved row"}
	}
	return nil
}

func saveStageWorktree(r *http.Request, tx *sql.Tx, e store.ContextExecution, w *resultWorktree) error {
	ctx := r.Context()
	copies, err := json.Marshal(w.CopiedFiles)
	if err != nil {
		return err
	}
	switch w.Action {
	case "created":
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worktrees WHERE id=? OR id=(SELECT worktree_id FROM executions WHERE id=?))`, w.ID, e.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return &Error{Code: Conflict, Message: "Execution already has a worktree or the ID is in use"}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO worktrees(id,project_id,task_id,runner_id,path,branch_name,base_sha,status,setup_status,copied_files,created_at)
   SELECT ?,t.project_id,e.task_id,e.runner_id,?,?,?,'active','pending',?,? FROM executions e JOIN tasks t ON t.id=e.task_id WHERE e.id=?`,
			w.ID, w.Path, w.BranchName, w.BaseSHA, string(copies), time.Now().UTC().Format(time.RFC3339Nano), e.ID)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE executions SET worktree_id=?,branch_name=? WHERE id=?`, w.ID, w.BranchName, e.ID); err != nil {
			return err
		}
	case "restore_started":
		if _, err = tx.ExecContext(ctx, `UPDATE worktrees SET copied_files='[]',setup_status='pending' WHERE id=?`, w.ID); err != nil {
			return err
		}
		return appendStageEvent(r, tx, e.ID, model.EventLog, map[string]string{"sourceId": "worktree-restore/" + w.ID, "stream": "runner", "text": "worktreeの復元を開始。コピーとセットアップの完了は未確認"})
	case "restored":
		var started bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_logs WHERE target_id=? AND action='execution.result' AND json_extract(detail,'$.step')='worktree.restore_started' AND json_extract(detail,'$.report.id')=?)`, e.ID, w.ID).Scan(&started); err != nil {
			return err
		}
		if !started {
			return &Error{Code: PreconditionFailed, Message: "Restore must be started before completion"}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE worktrees SET copied_files=? WHERE id=?`, string(copies), w.ID); err != nil {
			return err
		}
	}
	if err := appendStageEvent(r, tx, e.ID, model.EventWorktreeReady, map[string]string{"path": w.Path, "branch": w.BranchName, "baseSha": w.BaseSHA}); err != nil {
		return err
	}
	var command string
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(p.setup_command,'') FROM executions e JOIN tasks t ON t.id=e.task_id JOIN projects p ON p.id=t.project_id WHERE e.id=?`, e.ID).Scan(&command); err != nil {
		return err
	}
	if strings.TrimSpace(command) == "" {
		return appendStageEvent(r, tx, e.ID, model.EventLog, map[string]string{"sourceId": "worktree-setup-not-required/" + w.ID, "stream": "runner", "text": "セットアップコマンド未設定のため実行不要"})
	}
	return nil
}

func appendStageEvent(r *http.Request, tx *sql.Tx, id string, kind model.ExecutionEventType, payload any) error {
	encoded, err := json.Marshal(payload)
	if err == nil {
		_, err = store.AppendExecutionEvent(r.Context(), tx, id, kind, encoded)
	}
	return err
}
