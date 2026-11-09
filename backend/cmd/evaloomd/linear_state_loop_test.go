package main

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearintake"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/store"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

const loopCriteria = "## Acceptance Criteria\n- login works"

func attachLoopIssue(t *testing.T, h *loopHarness, body string, labels ...string) string {
	t.Helper()
	h.poll(t) // Complete the Team baseline before attaching the Issue.
	h.fake.set("issue-1", linearIssueJSON("issue-1", body,
		time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC), labels...))
	h.poll(t)
	var id string
	if err := h.db.QueryRow(`SELECT id FROM tasks WHERE idempotency_key = 'linear:issue-1:1'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Compare all columns, including timestamps and audit details, so a poll
// cannot silently rewrite an existing task or audit while keeping row counts.
func (h *loopHarness) effects(t *testing.T) map[string][][]any {
	t.Helper()
	result := map[string][][]any{}
	for _, table := range []string{"tasks", "executions", "audit_logs", "linear_outbox"} {
		result[table] = nil
		rows, err := h.db.Query(`SELECT * FROM ` + table + ` ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values, dest := make([]any, len(columns)), make([]any, len(columns))
			for i := range dest {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			for i, v := range values {
				if b, ok := v.([]byte); ok {
					values[i] = string(b)
				}
			}
			result[table] = append(result[table], values)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("snapshot %s: %v / %v", table, err, closeErr)
		}
	}
	return result
}

func (h *loopHarness) wantEffects(t *testing.T, before map[string][][]any) {
	t.Helper()
	for table, after := range h.effects(t) {
		if !reflect.DeepEqual(after, before[table]) {
			t.Errorf("%s changed after polling:\nbefore: %v\nafter:  %v", table, before[table], after)
		}
	}
}

func (h *loopHarness) wantStateSent(t *testing.T, name string, updates int) {
	t.Helper()
	state, changes := h.fake.stateSnapshot("issue-1")
	if state.Name != name || len(changes) != updates || len(changes) == 0 || changes[len(changes)-1]["stateId"] != state.ID {
		t.Fatalf("Linear state = %+v, updates = %v; want %s (%d updates)", state, changes, name, updates)
	}
	var sent int
	if err := h.db.QueryRow(`SELECT count(*) FROM linear_outbox
		WHERE action = 'set_state' AND state_name = ? AND status = 'sent'`, name).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("sent %s rows = %d, want 1", name, sent)
	}
}

// Delegate to the production Canceller while witnessing that the first poll
// really enters cancellation after Evaloom itself sets Canceled (16 3.5.4).
type loopCancellationProbe struct {
	linearintake.Canceller
	reasons []linearintake.CancelReason
}

func (p *loopCancellationProbe) CancelLinearTasks(ctx context.Context, tx *sql.Tx, id string, reason linearintake.CancelReason) error {
	p.reasons = append(p.reasons, reason)
	return p.Canceller.CancelLinearTasks(ctx, tx, id, reason)
}

func cancelLoopTask(t *testing.T, h *loopHarness, held bool) {
	t.Helper()
	body := loopCriteria
	if held {
		body = "no criteria"
	}
	id := attachLoopIssue(t, h, body, "ai-ready", "repo:p")
	// Deliver the hold comment first, if present, so only cancellation is due.
	h.send(t)
	tx, err := h.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := store.CancelTaskTx(context.Background(), tx, store.TaskCancellation{TaskID: id, Actor: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var status, actor string
	if err := h.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT actor FROM audit_logs WHERE action = 'task.cancel' AND target_id = ?`, id).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || actor != "owner" {
		t.Fatalf("owner cancellation = %s / %s", status, actor)
	}
	h.send(t)
	h.wantStateSent(t, "Canceled", 1)
}

// 受け入れ基準1: 本人の中止後に Canceled を照会して中止の経路に入っても、
// 終端のタスクと audit_logs は全列とも変わらない (16 3.5.4、15 3.7)。
func TestLinearOwnerCanceledStatePollLeavesTerminalTaskUntouched(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "queued"
		if held {
			name = "held without execution"
		}
		t.Run(name, func(t *testing.T) {
			h := newLoopHarness(t)
			cancelLoopTask(t, h, held)
			before, effects := h.state(t, "issue-1"), h.effects(t)
			intake := h.poller.Handler.(*linearintake.Intake)
			probe := &loopCancellationProbe{Canceller: intake.Canceller}
			intake.Canceller = probe
			for i := 0; i < 2; i++ {
				h.poll(t)
				if i == 0 && !reflect.DeepEqual(probe.reasons, []linearintake.CancelReason{linearintake.ReasonIssueCancelled}) {
					t.Fatalf("first poll cancellation reasons = %v, want linear_issue_cancelled", probe.reasons)
				}
				h.send(t)
				h.wantEffects(t, effects)
			}
			after := h.state(t, "issue-1")
			if !after.aiReady || after.seq != before.seq || after.decision != "cancelled" ||
				!sameInstant(t, after.updatedAt, h.fake.updatedAt("issue-1")) {
				t.Fatalf("polled Canceled state = %+v, before = %+v", after, before)
			}
			h.wantStateSent(t, "Canceled", 1)
		})
	}
}

// 受け入れ基準2: Evaloom が Canceled にした Issue で ai-ready の取り外しと
// 再付与を別々に照会しても、新しいタスクを作らない (16 3.6.4、15 3.4)。
func TestLinearReattachedCanceledIssueDoesNotStart(t *testing.T) {
	h := newLoopHarness(t)
	cancelLoopTask(t, h, false)
	before, effects := h.state(t, "issue-1"), h.effects(t)
	state, _ := h.fake.stateSnapshot("issue-1")
	at, err := time.Parse(time.RFC3339Nano, h.fake.updatedAt("issue-1"))
	if err != nil {
		t.Fatal(err)
	}
	for i, labels := range [][]string{{"repo:p"}, {"ai-ready", "repo:p"}} {
		issue := linearIssueJSON("issue-1", loopCriteria, at.Add(time.Duration(i+1)*time.Minute), labels...)
		issue["state"] = map[string]any{"id": state.ID, "name": state.Name, "type": state.Type}
		h.fake.set("issue-1", issue)
		h.poll(t)
		h.send(t)
		h.wantEffects(t, effects)
		got := h.state(t, "issue-1")
		wantDecision := "cancelled"
		if i == 1 {
			wantDecision = "skipped_cancelled"
		}
		if got.aiReady != (i == 1) || got.seq != before.seq || got.decision != wantDecision ||
			!sameInstant(t, got.updatedAt, h.fake.updatedAt("issue-1")) {
			t.Fatalf("after label change %d: %+v, want %s with seq %d", i, got, wantDecision, before.seq)
		}
	}
	h.poll(t)
	h.send(t)
	h.wantEffects(t, effects)
	h.wantStateSent(t, "Canceled", 1)
}

// 受け入れ基準3: running / waiting_approval / completed の実際の遷移が
// In Progress / PR Review / Done を送信して更新時刻が進んでも、ai-ready が
// 付いたままなら起動しない。Done では元のタスクが終端なので、アクティブな
// タスクの重複防止だけでは防げない再起動も検出する (16 3.5.4、3.6.4)。
func TestLinearWorkflowStateUpdatesDoNotRestartAttachedIssue(t *testing.T) {
	h := newLoopHarness(t)
	id := attachLoopIssue(t, h, loopCriteria, "ai-ready", "repo:p")
	var executionID string
	if err := h.db.QueryRow(`SELECT id FROM executions WHERE task_id = ?`, id).Scan(&executionID); err != nil {
		t.Fatal(err)
	}
	from := model.ExecutionPending
	for n, stage := range []struct {
		name  string
		steps []model.ExecutionStatus
	}{
		{"In Progress", []model.ExecutionStatus{model.ExecutionClaimed}},
		{"PR Review", []model.ExecutionStatus{model.ExecutionPreparing, model.ExecutionRunning,
			model.ExecutionVerifying, model.ExecutionPublishing, model.ExecutionWaitingApproval}},
		{"Done", []model.ExecutionStatus{model.ExecutionSucceeded}},
	} {
		t.Run(stage.name, func(t *testing.T) {
			for _, to := range stage.steps {
				fromTask, err := task.TaskStatusForExecution(from)
				if err != nil {
					t.Fatal(err)
				}
				if err := task.TransitionExecution(context.Background(), h.db, task.Transition{TaskID: id,
					ExecutionID: executionID, FromTask: fromTask, FromExecution: from, ToExecution: to}); err != nil {
					t.Fatal(err)
				}
				from = to
			}
			before := h.state(t, "issue-1")
			h.send(t)
			h.wantStateSent(t, stage.name, n+1)
			advanced := h.fake.updatedAt("issue-1")
			if sameInstant(t, advanced, before.updatedAt) {
				t.Fatal("state delivery did not advance updatedAt")
			}
			effects := h.effects(t)
			for i := 0; i < 2; i++ {
				h.poll(t)
				h.send(t)
				h.wantEffects(t, effects)
				got := h.state(t, "issue-1")
				if !got.aiReady || got.seq != before.seq || got.decision != "started" || !sameInstant(t, got.updatedAt, advanced) {
					t.Fatalf("state update restarted Issue: before=%+v after=%+v", before, got)
				}
			}
		})
		if t.Failed() {
			return // Later stages depend on this transition succeeding.
		}
	}
}

type loopRecheckProbe struct {
	linearintake.HeldRechecker
	calls int
}

func (p *loopRecheckProbe) RecheckHeldTask(ctx context.Context, tx *sql.Tx, issue linear.Issue) (linearintake.Decision, error) {
	p.calls++
	return p.HeldRechecker.RecheckHeldTask(ctx, tx, issue)
}

// 受け入れ基準4: 保留した Issue の状態だけが変わって再判定が走っても、
// 新しいタスクと同じ理由のコメントは作らない (16 3.5.3、3.6.4)。
func TestLinearHeldIssueStateUpdateDoesNotRepeatTaskOrComment(t *testing.T) {
	for _, hold := range []struct {
		cause, body, label string
	}{
		{"missing_acceptance_criteria", "no criteria", "repo:p"},
		{"routing_unknown_label", loopCriteria, "repo:unknown"},
	} {
		t.Run(hold.cause, func(t *testing.T) {
			h := newLoopHarness(t)
			id := attachLoopIssue(t, h, hold.body, "ai-ready", hold.label)
			h.send(t) // Deliver the initial hold comment.
			h.poll(t) // Consume that comment's updatedAt before changing state.
			var status, cause string
			if err := h.db.QueryRow(`SELECT status, intake_block_cause FROM tasks WHERE id = ?`, id).Scan(&status, &cause); err != nil {
				t.Fatal(err)
			}
			if status != "blocked" || cause != hold.cause {
				t.Fatalf("held task = %s / %s, want blocked / %s", status, cause, hold.cause)
			}
			comments, _ := h.fake.snapshot()
			if len(comments) != 1 {
				t.Fatalf("initial hold comments = %d, want 1", len(comments))
			}
			counts := h.counts(t)
			if counts.tasks != 1 || counts.executions != 0 || counts.outbox != 1 {
				t.Fatalf("initial hold counts = %+v, want 1 task, 0 executions and 1 comment row", counts)
			}
			effects := h.effects(t)
			intake := h.poller.Handler.(*linearintake.Intake)
			probe := &loopRecheckProbe{HeldRechecker: intake.Rechecker}
			intake.Rechecker = probe
			// A held task does not emit set_state; simulate an external state
			// change using the same issueUpdate API and updatedAt behavior.
			for n, state := range loopWorkflowStates[1:4] {
				t.Run(state.Name, func(t *testing.T) {
					before := h.state(t, "issue-1")
					if err := h.client.SetIssueState(context.Background(), "issue-1", state.ID); err != nil {
						t.Fatal(err)
					}
					advanced := h.fake.updatedAt("issue-1")
					if sameInstant(t, advanced, before.updatedAt) {
						t.Fatal("state change did not advance updatedAt")
					}
					for i := 0; i < 2; i++ {
						h.poll(t)
						h.send(t)
						h.wantEffects(t, effects)
					}
					got := h.state(t, "issue-1")
					if !got.aiReady || got.seq != before.seq || got.decision != "held" || !sameInstant(t, got.updatedAt, advanced) {
						t.Fatalf("held Issue after state change = %+v, before = %+v", got, before)
					}
					if probe.calls != n+1 {
						t.Fatalf("rechecks = %d, want %d (one per advanced updatedAt)", probe.calls, n+1)
					}
					if after, _ := h.fake.snapshot(); !reflect.DeepEqual(after, comments) {
						t.Fatalf("hold comments changed: before=%v after=%v", comments, after)
					}
				})
			}
		})
	}
}
