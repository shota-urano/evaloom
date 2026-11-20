package linearoutbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/shota-urano/evaloom/backend/internal/logredact"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// SummarySource reads out/summary.md (spec 16 3.3) of one Execution inside tx,
// from the work_summary artifact. ok=false means the summary is
// unavailable; it never fails the state transition that writes the row.
type SummarySource func(ctx context.Context, tx *sql.Tx, executionID string) (summary string, ok bool)

var (
	summaryMu     sync.RWMutex
	summarySource SummarySource
	remoteHost    string
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

// SetRemoteHost supplies evaloomd's validated -remote-host for management UI
// links (spec 16 3.3, Q122). An empty host leaves only IDs in the body.
func SetRemoteHost(host string) (restore func()) {
	summaryMu.Lock()
	previous := remoteHost
	remoteHost = host
	summaryMu.Unlock()
	return func() {
		summaryMu.Lock()
		remoteHost = previous
		summaryMu.Unlock()
	}
}

func managementReference(kind, id string) string {
	summaryMu.RLock()
	host := remoteHost
	summaryMu.RUnlock()
	if host == "" {
		return id
	}
	return "https://" + host + "/" + kind + "/" + url.PathEscape(id)
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
	model.TaskReceived: "Received", model.TaskRouting: "Routing", model.TaskQueued: "Queued",
	model.TaskRunning: "Running", model.TaskWaitingApproval: "Waiting Approval",
	model.TaskCompleted: "Completed", model.TaskFailed: "Failed", model.TaskCancelled: "Cancelled",
	model.TaskTimedOut: "Timed Out", model.TaskBlocked: "Blocked",
}

// executionFacts is what the body reads about the trigger's Execution.
type executionFacts struct {
	harnessID, profileVersion sql.NullString
	errorClass, errorMessage  sql.NullString
	projectName, outputs      sql.NullString
}

// Body builds linear_outbox.body for t inside tx (spec 16 3.3).
//
// It carries the applicable routing, work summary, check summaries and finding
// counts, outputs, error information and management UI references. A decision
// comment contains only a notice and its reference; no decision content.
//
// It never reads log events, log artifacts or test output, and never writes
// a path column. Free text written by the harness or the Runner (summary,
// error message) passes the log exclusion of spec 05 3.9 (logredact.Text);
// the summary was already excluded with the Runner's own secrets before the
// Runner stored it. Absolute local paths are excluded without persisting
// Runner directory configuration in the Control Plane.
// The body shows the task ID for the management UI (3.3) and ends with the
// dedupe_key used to match a comment whose send result is unknown (3.4).
func Body(ctx context.Context, tx *sql.Tx, t Trigger) (string, error) {
	if t.Kind == KindDecisionOpen {
		return decisionOpenBody(t), nil
	}
	var taskStatus model.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, t.TaskID).Scan(&taskStatus); err != nil {
		return "", fmt.Errorf("linearoutbox: read task status %s: %w", t.TaskID, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Evaloom: %s\n管理画面のタスク ID: %s\n\n", t.Kind, managementReference("tasks", t.TaskID))
	fmt.Fprintf(&b, "- 実行ステータス: %s\n", statusName(taskStatus))

	switch t.Kind {
	case KindIntakeBlocked, KindMissingAcceptanceCriteria:
		writeHold(&b, t)
	case KindSucceeded, KindFailed, KindCancelled:
		facts, ok, err := readExecution(ctx, tx, t)
		if err != nil {
			return "", err
		}
		if ok {
			if err := writeExecution(ctx, tx, &b, t, facts); err != nil {
				return "", err
			}
		}
	}
	if err := writeOpenDecisions(ctx, tx, &b, t.TaskID); err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "\n%s", t.DedupeKey())
	return b.String(), nil
}

// decisionOpenBody is the decision_open comment (spec 16 3.2): only that a
// decision waits in the management UI and the decision's reference (Q122).
// The decision's content (the approval target,
// findings, question text) never appears.
func decisionOpenBody(t Trigger) string {
	return fmt.Sprintf("Evaloom: %s\n管理画面で判断待ち\n判断待ち ID: %s\n\n%s", t.Kind, managementReference("decisions", t.TargetID), t.DedupeKey())
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
		e.error_class, e.error_message, p.name, e.outputs
		FROM executions e LEFT JOIN projects p ON p.id = e.project_id
		WHERE e.id = ? AND e.task_id = ?`, t.TargetID, t.TaskID).Scan(
		&f.harnessID, &f.profileVersion, &f.errorClass, &f.errorMessage, &f.projectName, &f.outputs)
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

func clean(text string) string {
	out, _ := logredact.Text(text, nil)
	out = localPathPattern.ReplaceAllStringFunc(out, func(match string) string {
		if strings.HasPrefix(match, "/") || strings.HasPrefix(strings.ToLower(match), "file://") {
			return "[REDACTED]"
		}
		return match
	})
	return strings.TrimSpace(out)
}

func writeExecution(ctx context.Context, tx *sql.Tx, b *strings.Builder, t Trigger, f executionFacts) error {
	if f.projectName.Valid {
		fmt.Fprintf(b, "- 振り分け先 Project: %s\n", clean(f.projectName.String))
	}
	if f.harnessID.Valid {
		fmt.Fprintf(b, "- 実行担当ハーネス: %s（Profile %s）\n", clean(f.harnessID.String), clean(f.profileVersion.String))
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
	if t.Kind != KindCancelled {
		summary, ok := readSummary(ctx, tx, t.TargetID)
		if ok {
			summary = clean(summary)
		}
		if ok && summary != "" {
			fmt.Fprintf(b, "\n作業概要:\n%s\n", summary)
		}
		if err := writeChecks(ctx, tx, b, t.TargetID); err != nil {
			return err
		}
		var outputs []struct {
			Label string `json:"label"`
			URL   string `json:"url"`
		}
		if f.outputs.Valid {
			if err := json.Unmarshal([]byte(f.outputs.String), &outputs); err != nil {
				return fmt.Errorf("linearoutbox: invalid outputs of execution %s", t.TargetID)
			}
		}
		if len(outputs) > 0 {
			b.WriteString("\n外に出したもの:\n")
			for _, output := range outputs {
				fmt.Fprintf(b, "- %s: %s\n", clean(output.Label), clean(output.URL))
			}
		}
	}
	return nil
}

// Only summaries and severity counts cross this boundary. Never copy the
// finding messages/locations, result JSON, or check-stage logs into comments.
func writeChecks(ctx context.Context, tx *sql.Tx, b *strings.Builder, executionID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT c.summary, c.findings FROM check_results c
		JOIN stage_runs s ON s.id = c.stage_run_id WHERE c.execution_id = ?
		ORDER BY s.attempt, c.id`, executionID)
	if err != nil {
		return fmt.Errorf("linearoutbox: read check results: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var summary sql.NullString
		var raw string
		if err := rows.Scan(&summary, &raw); err != nil {
			return err
		}
		var findings []struct {
			Severity string `json:"severity"`
		}
		if err := json.Unmarshal([]byte(raw), &findings); err != nil {
			return fmt.Errorf("linearoutbox: invalid findings of execution %s", executionID)
		}
		counts := make(map[string]int)
		for _, finding := range findings {
			counts[finding.Severity]++
		}
		b.WriteString("\n確かめた結果:\n")
		if text := clean(summary.String); text != "" {
			fmt.Fprintf(b, "%s\n", text)
		}
		fmt.Fprintf(b, "finding 件数: error=%d, warning=%d, info=%d\n", counts["error"], counts["warning"], counts["info"])
	}
	return rows.Err()
}

func writeOpenDecisions(ctx context.Context, tx *sql.Tx, b *strings.Builder, taskID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM owner_decisions
		WHERE task_id = ? AND status = 'open' ORDER BY created_at, id`, taskID)
	if err != nil {
		return fmt.Errorf("linearoutbox: read open decisions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		fmt.Fprintf(b, "- 判断待ち ID: %s\n", managementReference("decisions", id))
	}
	return rows.Err()
}

// Runner directories are local configuration. Without a DB copy, exclude
// absolute paths regardless of the preceding character. Match scheme:// URLs
// first so that their slashes are preserved rather than treated as local paths.
// \x60 is the backtick delimiter for Markdown inline code.
var localPathPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s\x60"')\]）]+|/[^\s\x60"')\]）]*`)
