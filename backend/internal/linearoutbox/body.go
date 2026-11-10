package linearoutbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
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
	harnessID, profileVersion sql.NullString
	errorClass, errorMessage  sql.NullString
}

// Body builds linear_outbox.body for t inside tx (spec 16 3.3).
//
// It carries the requirements 6.4 items that apply to the trigger (3.2):
// 実行ステータス, 実行担当ハーネス, 作業概要, エラー情報.
// The waiting_approval body carries 必要な承認事項: the operation and the
// target of the Execution's pending approval (spec 17 3.4), and that the
// owner approves in the management UI (3.2). An intake hold carries the
// reason and the fix instead (3.2); the missing-criteria hold also asks for
// the criteria.
//
// It never reads log events, log artifacts or test output, and never writes
// a path column. Free text written by the harness or the Runner (summary,
// error message) passes the log exclusion of spec 05 3.5 (logredact.Text);
// the summary was already excluded with the Runner's own secrets before the
// Runner stored it. Absolute local paths are excluded without persisting
// Runner directory configuration in the Control Plane.
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
	case KindIntakeBlocked, KindMissingAcceptanceCriteria:
		writeHold(&b, t)
	case KindWaitingApproval:
		writeApproval(ctx, tx, &b, t)
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

// holdTexts are the reason and the fix of each intake hold (spec 16 3.2,
// spec 15 3.6). {prefix} is the routing label prefix.
var holdTexts = map[string]struct{ reason, fix string }{
	"routing_multiple_labels": {
		"実行先ラベル（{prefix} で始まるラベル）が2つ以上付いているため、実行先を決められない",
		"実行先ラベルを1つにする"},
	"routing_unknown_label": {
		"実行先ラベル（{prefix} で始まるラベル）が Evaloom に登録されていない",
		"Evaloom に登録済みの実行先ラベルを付ける"},
	"routing_team_mismatch": {
		"実行先ラベル（{prefix} で始まるラベル）は登録済みだが、その Project の Linear Team がこの Issue の Team と違う",
		"この Issue の Team で登録済みの実行先ラベルを付ける"},
	"routing_no_label": {
		"実行先ラベル（{prefix} で始まるラベル）が付いていない",
		"Evaloom に登録済みの実行先ラベルを1つ付ける"},
	"missing_acceptance_criteria": {
		"Issue の本文に受け入れ条件が書かれていない",
		"本文に「受け入れ条件」または「Acceptance Criteria」という見出し（例: `## 受け入れ条件`）を作り、その下に受け入れ条件を書く"},
	"config_missing_assignment": {
		"実行先の Project の default role にハーネスが割り当てられていない",
		"管理画面で Project の default role にハーネスを割り当てる"},
}

// writeHold writes why the task is held at intake and how to fix it. The
// trigger's target is the intake_block_cause. The text is fixed, so it
// carries no secret.
func writeHold(b *strings.Builder, t Trigger) {
	label := "（" + t.RoutingLabelPrefix + " で始まるラベル）"
	if t.RoutingLabelPrefix == "" {
		label = ""
	}
	fill := func(s string) string { return strings.ReplaceAll(s, "（{prefix} で始まるラベル）", label) }
	text, ok := holdTexts[t.TargetID]
	if !ok {
		fmt.Fprintf(b, "- 保留の理由: %s\n", t.TargetID)
		return
	}
	if t.Kind == KindMissingAcceptanceCriteria {
		b.WriteString("\n受け入れ条件の記入をお願いします。\n")
	}
	fmt.Fprintf(b, "- 保留の理由: %s\n", fill(text.reason))
	fmt.Fprintf(b, "- 直し方: %s\n", fill(text.fix))
	b.WriteString("- Issue を直すと、次の照会で Evaloom が判定をやり直す\n")
}

// writeApproval writes 必要な承認事項 (spec 16 3.3) from the Execution's
// unused approvals: the operation. The target stays in the management UI.
func writeApproval(ctx context.Context, tx *sql.Tx, b *strings.Builder, t Trigger) {
	rows, err := tx.QueryContext(ctx, `SELECT operation
		FROM approvals WHERE execution_id = ? AND status IN ('pending', 'approved') ORDER BY created_at, id`, t.TargetID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var operation string
			if rows.Scan(&operation) != nil {
				break
			}
			fmt.Fprintf(b, "- 必要な承認事項: %s\n", operation)
		}
	}
	b.WriteString("- 承認・却下は管理画面で本人が行う\n")
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
	err := tx.QueryRowContext(ctx, `SELECT e.harness_id, e.profile_version,
		e.error_class, e.error_message
		FROM executions e WHERE e.id = ? AND e.task_id = ?`, t.TargetID, t.TaskID).Scan(
		&f.harnessID, &f.profileVersion, &f.errorClass, &f.errorMessage)
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
	clean := func(text string) string {
		out, _ := logredact.Text(text, nil)
		out = localPathPattern.ReplaceAllString(out, "$1[REDACTED]")
		return strings.TrimSpace(out)
	}
	if f.harnessID.Valid {
		fmt.Fprintf(b, "- 実行担当ハーネス: %s（Profile %s）\n", f.harnessID.String, f.profileVersion.String)
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
		b.WriteString("- 続行・中止は管理画面で本人が選ぶ\n")
	}
	if t.Kind != KindCancelled {
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

// Runner directories are local configuration. Without a DB copy, exclude
// absolute paths in free text while preserving URL paths (preceded by ':').
var localPathPattern = regexp.MustCompile(`(^|[\s"'(])(/[^\s"')]+)`)
