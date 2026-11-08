package linearoutbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/shota-urano/evaloom/backend/internal/logredact"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// SummarySource reads HarnessResult.summary (spec 02 5.8) of one Execution
// inside tx, from the harness_output artifact. ok=false means the summary is
// unavailable; it never fails the state transition that writes the row.
type SummarySource func(ctx context.Context, tx *sql.Tx, executionID string) (summary string, ok bool)

var (
	summaryMu     sync.RWMutex
	summarySource SummarySource
)

// SetSummarySource installs the artifact reader used for 作業概要. evaloomd
// installs the Control Plane's ArtifactStore. The returned function restores
// the previous source. Without a source the summary is unavailable.
func SetSummarySource(source SummarySource) (restore func()) {
	summaryMu.Lock()
	previous := summarySource
	summarySource = source
	summaryMu.Unlock()
	return func() {
		summaryMu.Lock()
		summarySource = previous
		summaryMu.Unlock()
	}
}

func readSummary(ctx context.Context, tx *sql.Tx, executionID string) (string, bool) {
	summaryMu.RLock()
	source := summarySource
	summaryMu.RUnlock()
	if source == nil {
		return "", false
	}
	return source(ctx, tx, executionID)
}

// taskStatusNames are the display names of spec 02 5.1.
var taskStatusNames = map[model.TaskStatus]string{
	model.TaskReceived: "Received", model.TaskPlanning: "Planning", model.TaskQueued: "Queued",
	model.TaskRunning: "Running", model.TaskVerifying: "Verifying", model.TaskWaitingApproval: "Waiting Approval",
	model.TaskCompleted: "Completed", model.TaskFailed: "Failed", model.TaskCancelled: "Cancelled",
	model.TaskTimedOut: "Timed Out", model.TaskBlocked: "Blocked",
}

// executionFacts is what the body reads about the trigger's Execution.
type executionFacts struct {
	harnessID, profileVersion       sql.NullString
	prURL, pushedSHA                sql.NullString
	errorClass, errorMessage        sql.NullString
	worktreePath, runnerWorktreeDir sql.NullString
}

// Body builds linear_outbox.body for t inside tx (spec 16 3.3).
//
// It carries the requirements 6.4 items that apply to the trigger (3.2):
// 実行ステータス, 実行担当ハーネス, 作業概要, 検証結果, PR URL, エラー情報.
// 必要な承認事項 has no data source yet (spec 17 is not implemented), so the
// waiting_approval body carries the status only.
//
// It never reads log events, log artifacts or test output, and never writes
// a path column. Free text written by the harness or the Runner (summary,
// error message) passes the log exclusion of spec 05 3.5 (logredact.Text);
// the summary was already excluded with the Runner's own secrets before the
// Runner stored it. The Execution's worktree path and the Runner's worktree
// root are excluded from that text as literals, so Runner paths do not leak.
// The body shows the task ID for the management UI (3.3) and ends with the
// dedupe_key used to match a comment whose send result is unknown (3.4).
func Body(ctx context.Context, tx *sql.Tx, t Trigger) (string, error) {
	var taskStatus model.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, t.TaskID).Scan(&taskStatus); err != nil {
		return "", fmt.Errorf("linearoutbox: read task status %s: %w", t.TaskID, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Evaloom: %s\n管理画面のタスク ID: %s\n\n", t.Kind, t.TaskID)
	fmt.Fprintf(&b, "- 実行ステータス: %s\n", statusName(taskStatus))

	switch t.Kind {
	case KindSucceeded, KindFailed, KindBlocked, KindCancelled:
		facts, ok, err := readExecution(ctx, tx, t)
		if err != nil {
			return "", err
		}
		if ok {
			writeExecution(ctx, tx, &b, t, facts)
		}
	}
	fmt.Fprintf(&b, "\n%s", t.DedupeKey())
	return b.String(), nil
}

func statusName(status model.TaskStatus) string {
	if name, ok := taskStatusNames[status]; ok {
		return name
	}
	return string(status)
}

// readExecution reads the Execution named by the trigger. A cancelled task
// held at intake has no Execution (its target is the task); ok is false then.
func readExecution(ctx context.Context, tx *sql.Tx, t Trigger) (executionFacts, bool, error) {
	var f executionFacts
	err := tx.QueryRowContext(ctx, `SELECT e.harness_id, e.profile_version, e.pr_url, e.pushed_sha,
		e.error_class, e.error_message,
		(SELECT w.path FROM worktrees w WHERE w.id = e.worktree_id),
		(SELECT r.worktree_root FROM runners r WHERE r.id = e.runner_id)
		FROM executions e WHERE e.id = ? AND e.task_id = ?`, t.TargetID, t.TaskID).Scan(
		&f.harnessID, &f.profileVersion, &f.prURL, &f.pushedSHA, &f.errorClass, &f.errorMessage,
		&f.worktreePath, &f.runnerWorktreeDir)
	if errors.Is(err, sql.ErrNoRows) {
		if t.Kind == KindCancelled && t.TargetID == t.TaskID {
			return f, false, nil
		}
		return f, false, fmt.Errorf("linearoutbox: execution %s of task %s is missing", t.TargetID, t.TaskID)
	}
	if err != nil {
		return f, false, fmt.Errorf("linearoutbox: read execution %s: %w", t.TargetID, err)
	}
	return f, true, nil
}

func writeExecution(ctx context.Context, tx *sql.Tx, b *strings.Builder, t Trigger, f executionFacts) {
	var paths []string
	for _, path := range []sql.NullString{f.worktreePath, f.runnerWorktreeDir} {
		if path.Valid && path.String != "" {
			paths = append(paths, path.String)
		}
	}
	clean := func(text string) string {
		out, _ := logredact.Text(text, paths)
		return strings.TrimSpace(out)
	}
	if f.harnessID.Valid {
		fmt.Fprintf(b, "- 実行担当ハーネス: %s（Profile %s）\n", f.harnessID.String, f.profileVersion.String)
	}
	if t.Kind == KindSucceeded {
		switch {
		case f.prURL.Valid && f.prURL.String != "":
			fmt.Fprintf(b, "- PR URL: %s\n", f.prURL.String)
		case f.pushedSHA.Valid && f.pushedSHA.String != "":
			fmt.Fprintf(b, "- PR URL: なし（ベースブランチへ push した commit: %s）\n", f.pushedSHA.String)
		}
	}
	if t.Kind == KindFailed || t.Kind == KindCancelled {
		if f.errorClass.Valid && f.errorClass.String != "" {
			fmt.Fprintf(b, "- エラー情報: %s", f.errorClass.String)
			if message := clean(f.errorMessage.String); message != "" {
				fmt.Fprintf(b, "（%s）", strings.Join(strings.Fields(message), " "))
			}
			b.WriteString("\n")
		}
	}
	if t.Kind == KindBlocked {
		var cause sql.NullString
		// The undecided block is the one this transition stopped at.
		_ = tx.QueryRowContext(ctx, `SELECT cause FROM execution_blocks WHERE execution_id = ?
			AND decision IS NULL ORDER BY created_at DESC, id DESC LIMIT 1`, t.TargetID).Scan(&cause)
		if cause.Valid {
			fmt.Fprintf(b, "- エラー情報: Block 原因 %s\n", cause.String)
		}
		b.WriteString("- 続行・中止は管理画面で本人が選ぶ\n")
	}
	if t.Kind != KindCancelled {
		writeVerification(ctx, tx, b, t.TargetID)
		summary, ok := readSummary(ctx, tx, t.TargetID)
		if ok {
			summary = clean(summary)
		}
		switch {
		case ok && summary != "":
			fmt.Fprintf(b, "\n作業概要:\n%s\n", summary)
		case t.Kind == KindSucceeded:
			b.WriteString("\n作業概要: 取得できなかった\n")
		}
	}
}

// writeVerification writes 検証結果: Gate 2 run and skip counts with the
// confirmation method, and the Gate 3 result (spec 16 3.3), using the same
// wording as the PR body (spec 11 3.3). Finding messages are not copied.
// Nothing is written when no Gate ran.
func writeVerification(ctx context.Context, tx *sql.Tx, b *strings.Builder, executionID string) {
	latest := func(gate int) (result string, findings []model.Finding, ok bool) {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT result, findings FROM gate_results WHERE execution_id = ? AND gate = ?
			ORDER BY created_at DESC, id DESC LIMIT 1`, executionID, gate).Scan(&result, &raw)
		if err != nil {
			return "", nil, false
		}
		_ = json.Unmarshal([]byte(raw), &findings)
		return result, findings, true
	}
	gate2, findings, ok2 := latest(2)
	gate3, _, ok3 := latest(3)
	if !ok2 && !ok3 {
		return
	}
	b.WriteString("\n検証結果:\n")
	if ok2 {
		var total, skipped sql.NullInt64
		_ = tx.QueryRowContext(ctx, `SELECT total, skipped FROM test_runs WHERE execution_id = ? AND kind = 'verify'
			ORDER BY started_at DESC, id DESC LIMIT 1`, executionID).Scan(&total, &skipped)
		counts := "実行数・skip 数: 取得できなかった"
		if total.Valid && skipped.Valid {
			counts = fmt.Sprintf("実行数: %d、skip 数: %d", total.Int64-skipped.Int64, skipped.Int64)
		}
		method := "ID による照合"
		for _, finding := range findings {
			if finding.Rule == "count_only_check" {
				method = "件数による確認"
			}
		}
		for _, finding := range findings {
			if finding.Rule == "test_report_missing" {
				method = "照合できない"
			}
		}
		fmt.Fprintf(b, "- Gate 2: %s（%s、確認方法: %s）\n", gate2, counts, method)
	}
	if ok3 {
		fmt.Fprintf(b, "- Gate 3: %s\n", gate3)
	}
}
