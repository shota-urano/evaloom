package linearintake

import (
	"database/sql"
	"testing"
	"time"
)

// outboxKeys returns the task's linear_outbox rows as kind -> dedupe_key,
// and fails on a row that is not a pending comment for Issue "1".
func outboxKeys(t *testing.T, db *sql.DB, taskID string) map[string][]string {
	t.Helper()
	rows, err := db.Query(`SELECT kind, dedupe_key, linear_issue_id, status FROM linear_outbox
		WHERE task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][]string{}
	for rows.Next() {
		var kind, key, issue, status string
		if err := rows.Scan(&kind, &key, &issue, &status); err != nil {
			t.Fatal(err)
		}
		if issue != "1" || status != "pending" {
			t.Fatalf("outbox row %s: issue %q status %q", key, issue, status)
		}
		got[kind] = append(got[kind], key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// spec 16 3.2: a routing hold and a missing-criteria hold each write their
// comment in the hold's transaction; a routed Issue writes none.
func TestHoldWritesOutboxRow(t *testing.T) {
	f := newRecheckFixture(t)
	held := f.hold(t, routedIssue("1", "repo:unknown"), CauseRoutingUnknownLabel)
	got := outboxKeys(t, f.db, held.id)
	if len(got) != 1 || len(got["intake_blocked"]) != 1 ||
		got["intake_blocked"][0] != held.id+":intake_blocked:routing_unknown_label" {
		t.Fatalf("outbox = %v", got)
	}

	f2 := newRecheckFixture(t)
	issue := routedIssue("1", "repo:one")
	issue.Description = "no criteria"
	held2 := f2.hold(t, issue, CauseMissingAcceptanceCriteria)
	got = outboxKeys(t, f2.db, held2.id)
	if len(got) != 1 || len(got["missing_acceptance_criteria"]) != 1 ||
		got["missing_acceptance_criteria"][0] != held2.id+":missing_acceptance_criteria:missing_acceptance_criteria" {
		t.Fatalf("outbox = %v", got)
	}

	f3 := newRecheckFixture(t)
	f3.mustRound(t, routedIssue("1", "repo:one"))
	if n := countRows(t, f3.db, `SELECT count(*) FROM linear_outbox`); n != 0 {
		t.Fatalf("started task wrote %d outbox rows", n)
	}
}

// spec 16 3.5 rule 3: a recheck with the same cause writes no second
// comment; a different cause writes one for the new cause; a release
// writes none.
func TestRecheckOutboxRows(t *testing.T) {
	f := newRecheckFixture(t)
	held := f.hold(t, routedIssue("1", "repo:unknown"), CauseRoutingUnknownLabel)

	same := routedIssue("1", "repo:other")
	same.UpdatedAt = base.Add(time.Minute)
	f.mustRound(t, same)
	if got := outboxKeys(t, f.db, held.id); len(got["intake_blocked"]) != 1 {
		t.Fatalf("same cause outbox = %v", got)
	}

	changed := routedIssue("1", "repo:one", "repo:three")
	changed.UpdatedAt = base.Add(2 * time.Minute)
	f.mustRound(t, changed)
	got := outboxKeys(t, f.db, held.id)
	if len(got["intake_blocked"]) != 2 || got["intake_blocked"][1] != held.id+":intake_blocked:routing_multiple_labels" {
		t.Fatalf("changed cause outbox = %v", got)
	}

	fixed := routedIssue("1", "repo:one")
	fixed.UpdatedAt = base.Add(3 * time.Minute)
	f.mustRound(t, fixed)
	if got := outboxKeys(t, f.db, held.id); len(got) != 1 || len(got["intake_blocked"]) != 2 {
		t.Fatalf("released task outbox = %v", got)
	}
}

// spec 16 3.2: a Linear-caused cancellation comments, both for a held task
// (no Execution: keyed on the task) and for a queued task (keyed on its
// Execution). The owner's cancellation takes the same CancelTaskTx path.
func TestCancellationWritesOutboxRow(t *testing.T) {
	for _, state := range []string{"held", "queued", "blocked"} {
		t.Run(state, func(t *testing.T) {
			f := newCancelFixture(t)
			issue, taskID := f.attach(t, state)
			before := outboxKeys(t, f.db, taskID)
			detachTriggers[0].run(t, f, issue)
			got := outboxKeys(t, f.db, taskID)
			want := taskID
			if state != "held" {
				if err := f.db.QueryRow(`SELECT id FROM executions WHERE task_id = ?`, taskID).Scan(&want); err != nil {
					t.Fatal(err)
				}
			}
			if len(got["cancelled"]) != 1 || got["cancelled"][0] != taskID+":cancelled:"+want ||
				len(got)-len(before) != 1 {
				t.Fatalf("outbox = %v (before %v)", got, before)
			}
		})
	}
}
