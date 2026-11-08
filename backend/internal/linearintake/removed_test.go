package linearintake

import (
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
)

// Tests for the removed mark of linear_issue_states (spec 15 3.4, 3.7, 3.8
// removed_at, Q78), run on the real TaskStarter and TaskCanceller.

func countAllTasks(t *testing.T, f *cancelFixture) (tasks, executions int) {
	t.Helper()
	if err := f.db.QueryRow(`SELECT (SELECT count(*) FROM tasks), (SELECT count(*) FROM executions)`).
		Scan(&tasks, &executions); err != nil {
		t.Fatal(err)
	}
	return tasks, executions
}

var removalTriggers = []struct {
	name string
	run  func(t *testing.T, f *cancelFixture, issue linear.Issue)
}{
	{"deleted", func(t *testing.T, f *cancelFixture, issue linear.Issue) {
		delete(f.fetcher.issues, issue.ID) // the refetch finds nothing
		f.mustRound(t)
	}},
	{"archived", func(t *testing.T, f *cancelFixture, issue linear.Issue) {
		issue.Archived = true
		f.fetcher.issues[issue.ID] = issue
		f.mustRound(t)
	}},
	{"trashed", func(t *testing.T, f *cancelFixture, issue linear.Issue) {
		issue.Trashed = true
		f.fetcher.issues[issue.ID] = issue
		f.mustRound(t)
	}},
}

// 受け入れ基準: 削除・アーカイブを検出すると中止の経路に入り、linear_issue_states の行が残って
// ai_ready=0・removed_at が入り、attach_seq が変わらない。
func TestRemovedIssueKeepsRowWithMark(t *testing.T) {
	for _, trigger := range removalTriggers {
		t.Run(trigger.name, func(t *testing.T) {
			f := newCancelFixture(t)
			issue, taskID := f.attach(t, "queued")
			before := mustState(t, f.db, issue.ID)
			if before.seq != 1 || before.removedAt != "" {
				t.Fatalf("attached row = %+v", before)
			}
			f.intake.Now = func() time.Time { return base.Add(time.Hour) }
			trigger.run(t, f, issue)

			if got := readCancelled(t, f.db, taskID); got.task != "cancelled" || got.cancelAudits != 1 {
				t.Fatalf("task = %+v, want cancelled once", got)
			}
			assertSystemAudit(t, f.db, taskID, ReasonIssueRemoved, false)
			s, ok := readState(t, f.db, issue.ID)
			if !ok {
				t.Fatal("linear_issue_states row deleted")
			}
			if s.aiReady || s.seq != before.seq || s.removedAt != base.Add(time.Hour).Format(time.RFC3339Nano) ||
				s.decision != string(DecisionCancelled) {
				t.Fatalf("row = %+v, want ai_ready=0, attach_seq %d, removed_at set, cancelled", s, before.seq)
			}
			// A marked row is not attached: it is not refetched every round.
			f.fetcher.calls = nil
			f.mustRound(t)
			if len(f.fetcher.calls) != 0 {
				t.Fatalf("removed issue refetched: %v", f.fetcher.calls)
			}
		})
	}
}

// 受け入れ基準: その Issue が ai-ready 付きのまま一覧に再び現れると、行が ai_ready=1・removed_at が
// NULL・last_decision=skipped_restored になり、タスクも Execution も作られない;
// 状態がキャンセルのまま再び現れても同じになる。
func TestRestoredIssueIsRecordedWithoutStarting(t *testing.T) {
	for _, trigger := range removalTriggers {
		for _, state := range []string{"unstarted", "canceled"} {
			t.Run(trigger.name+"/"+state, func(t *testing.T) {
				f := newCancelFixture(t)
				issue, _ := f.attach(t, "queued")
				if state == "canceled" {
					// Cancelled first, then removed (Linear archives cancelled Issues).
					issue.StateType = "canceled"
					f.mustRound(t, issue)
				}
				trigger.run(t, f, issue)
				tasks, executions := countAllTasks(t, f)
				audits := countCancelAudits(t, f)

				restored := issue
				restored.Archived, restored.Trashed = false, false
				restored.UpdatedAt = base.Add(2 * time.Hour)
				f.fetcher.issues[issue.ID] = restored
				for range 2 { // the reappearance and the round after it
					f.mustRound(t, restored)
					s := mustState(t, f.db, issue.ID)
					if !s.aiReady || s.removedAt != "" || s.seq != 1 || s.decision != string(DecisionSkippedRestored) {
						t.Fatalf("restored row = %+v, want ai_ready=1, removed_at NULL, attach_seq 1, skipped_restored", s)
					}
					if gotTasks, gotExecutions := countAllTasks(t, f); gotTasks != tasks || gotExecutions != executions {
						t.Fatalf("tasks/executions = %d/%d, want %d/%d", gotTasks, gotExecutions, tasks, executions)
					}
					if got := countCancelAudits(t, f); got != audits {
						t.Fatalf("cancel audits = %d, want %d (the removal is not cancelled again)", got, audits)
					}
				}
			})
		}
	}
}

func countCancelAudits(t *testing.T, f *cancelFixture) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action = 'task.cancel'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 受け入れ基準: その後に ai-ready を外した照会で ai_ready=0 になり、付け直した照会で attach_seq が
// 1増えて新しい冪等キーのタスクができる。
func TestRestoredIssueReattachStartsNewTask(t *testing.T) {
	for _, trigger := range removalTriggers {
		t.Run(trigger.name, func(t *testing.T) {
			f := newCancelFixture(t)
			issue, firstTask := f.attach(t, "queued")
			trigger.run(t, f, issue)
			f.fetcher.issues[issue.ID] = issue
			f.mustRound(t, issue) // restored: skipped_restored

			detached := issue
			detached.Labels = []string{"repo:one"}
			f.fetcher.issues[issue.ID] = detached
			f.mustRound(t)
			if s := mustState(t, f.db, issue.ID); s.aiReady || s.seq != 1 || s.removedAt != "" {
				t.Fatalf("detached row = %+v, want ai_ready=0 attach_seq 1", s)
			}

			f.mustRound(t, issue)
			s := mustState(t, f.db, issue.ID)
			if !s.aiReady || s.seq != 2 || s.decision != string(DecisionStarted) {
				t.Fatalf("reattached row = %+v, want ai_ready=1 attach_seq 2 started", s)
			}
			second := readStarted(t, f.db, issue.ID, 2) // idempotency key linear:1:2
			if second.id == firstTask || second.status != "queued" || second.executions != 1 {
				t.Fatalf("new task = %+v (first %s)", second, firstTask)
			}
		})
	}
}

// 受け入れ基準: 削除済みの印が無い行の付与・取り外しの判定は変わらない。
// Each unmarked case of the 3.4 tables gives the same decision as before
// Q78, and removed_at stays NULL.
func TestUnmarkedRowsDecideAsBefore(t *testing.T) {
	f := newCancelFixture(t)
	assertRow := func(id string, aiReady bool, seq int, decision Decision) {
		t.Helper()
		s := mustState(t, f.db, id)
		if s.aiReady != aiReady || s.seq != seq || s.decision != string(decision) || s.removedAt != "" {
			t.Fatalf("%s: %+v, want ai_ready=%v seq=%d %s removed_at NULL", id, s, aiReady, seq, decision)
		}
	}
	a := routedIssue("a", "repo:one")
	cancelled := routedIssue("c", "repo:one")
	cancelled.StateType = "canceled"
	f.mustRound(t, a, cancelled)
	assertRow("a", true, 1, DecisionStarted)          // no row: attach
	assertRow("c", true, 0, DecisionSkippedCancelled) // no row, cancelled: record only
	f.mustRound(t, a, cancelled)
	assertRow("a", true, 1, DecisionStarted) // still attached: nothing

	detached := a
	detached.Labels = []string{"repo:one"}
	f.fetcher.issues["a"] = detached
	f.mustRound(t, cancelled)
	assertRow("a", false, 1, DecisionCancelled) // detach
	if got := readStarted(t, f.db, "a", 1); got.status != "cancelled" {
		t.Fatalf("detached task = %+v", got)
	}
	f.mustRound(t, a, cancelled)
	assertRow("a", true, 2, DecisionStarted) // ai_ready=0 without a mark: attach again
	readStarted(t, f.db, "a", 2)

	cancelledA := a
	cancelledA.StateType = "canceled"
	f.mustRound(t, cancelledA, cancelled)
	assertRow("a", true, 2, DecisionCancelled) // attached and cancelled: stop
	if got := readStarted(t, f.db, "a", 2); got.status != "cancelled" {
		t.Fatalf("cancelled task = %+v", got)
	}
}
