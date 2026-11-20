package linearintake

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Each destination uses a selected Profile while the other pin points to a
// Profile with the opposite declaration. This catches consulting the wrong
// version for a Project or a direct special-harness routing (spec 19 3.9).
func criteriaDestination(t *testing.T, db *sql.DB, kind, policy string) string {
	t.Helper()
	exec(t, db, `INSERT INTO harness_profiles
		(id,harness_id,version,commit_sha,tag_name,contract_version,definition,definition_hash,model_id,operation_status,registered_at)
		SELECT 'optional-profile',harness_id,'2.0.0',commit_sha,'profile/v2.0.0',contract_version,
		json_set(definition, '$.acceptanceCriteria', 'optional'),definition_hash,model_id,operation_status,registered_at
		FROM harness_profiles WHERE id='fixture-profile'`)
	selected, other := "fixture-profile", "optional-profile"
	if policy == "optional" {
		selected, other = other, selected
	}
	project, direct := selected, other
	label := "route:one"
	if kind == "harness" {
		project, direct = other, selected
		label = "route:direct"
		exec(t, db, `INSERT INTO routes VALUES (?,NULL,'fixture-harness','t','t')`, label)
	}
	exec(t, db, `UPDATE projects SET profile_id=? WHERE id='p1'`, project)
	exec(t, db, `UPDATE harnesses SET default_profile_id=? WHERE id='fixture-harness'`, direct)
	return label
}

// Acceptance criteria 1 and 2: required holds without an Execution and asks
// for criteria; optional queues with an empty string. Existing headings are
// extracted for either policy. Both destination kinds use their own Profile.
func TestLinearAcceptanceCriteriaFollowsDestinationProfile(t *testing.T) {
	for _, kind := range []string{"project", "harness"} {
		for _, policy := range []string{"required", "optional"} {
			for _, body := range []struct{ name, text, criteria string }{
				{"no heading", "Do the work.", ""},
				{"blank section", "## Acceptance Criteria\n \t\u3000\n## Notes\ntext", ""},
				{"English heading", criteriaBody, "- login works\n- tests pass"},
				{"Japanese heading", "本文\n## 受け入れ条件\n- 動く\n## 備考\ntext", "- 動く"},
			} {
				t.Run(kind+"/"+policy+"/"+body.name, func(t *testing.T) {
					db := startDB(t)
					label := criteriaDestination(t, db, kind, policy)
					issue := routedIssue("1", label)
					issue.Description = body.text
					decision, err := start(t, db, issue, 1)
					if err != nil {
						t.Fatal(err)
					}
					got := readStarted(t, db, "1", 1)
					status, cause, executions, wantDecision := "queued", "", 1, DecisionStarted
					if policy == "required" && body.criteria == "" {
						status, cause, executions, wantDecision = "blocked", "missing_acceptance_criteria", 0, DecisionHeld
					}
					if decision != wantDecision || got.status != status || got.cause.String != cause ||
						got.cause.Valid != (cause != "") || got.criteria != body.criteria || got.executions != executions {
						t.Fatalf("decision=%q task=%+v; want %q/%q criteria=%q executions=%d", decision, got, status, cause, body.criteria, executions)
					}
					var harness sql.NullString
					if err := db.QueryRow(`SELECT harness_id FROM tasks WHERE id=?`, got.id).Scan(&harness); err != nil {
						t.Fatal(err)
					}
					if kind == "project" && (got.project.String != "p1" || harness.Valid) ||
						kind == "harness" && (got.project.Valid || harness.String != "fixture-harness") {
						t.Fatalf("wrong destination: project=%v harness=%v", got.project, harness)
					}
					comments := outboxKeys(t, db, got.id)
					if cause != "" {
						if len(comments) != 1 || len(comments[cause]) != 1 {
							t.Fatalf("missing criteria comment: %v", comments)
						}
					} else if len(comments) != 0 {
						t.Fatalf("unexpected hold comment: %v", comments)
					}
				})
			}
		}
	}
}

// Acceptance criterion 3: no label, an unknown label or multiple labels must
// stay routing without reading a Profile or holding for missing criteria.
func TestRoutingDoesNotCheckAcceptanceCriteria(t *testing.T) {
	for _, labels := range [][]string{nil, {"route:unknown"}, {"route:one", "route:two"}} {
		t.Run("labels="+strings.Join(labels, ","), func(t *testing.T) {
			db := startDB(t)
			// Reading this declaration would fail, proving routing skips it.
			exec(t, db, `UPDATE harness_profiles SET definition='{"acceptanceCriteria":7}'`)
			issue := routedIssue("1", labels...)
			issue.Description = "No criteria heading."
			if _, err := start(t, db, issue, 1); err != nil {
				t.Fatal(err)
			}
			got := readStarted(t, db, "1", 1)
			if got.status != "routing" || got.cause.Valid || got.executions != 0 || got.criteria != "" || got.project.Valid {
				t.Fatalf("task=%+v", got)
			}
			if comments := outboxKeys(t, db, got.id); len(comments) != 0 {
				t.Fatalf("routing wrote hold comments: %v", comments)
			}
		})
	}
}

func TestAcceptanceCriteriaRecheckUsesCurrentProfile(t *testing.T) {
	for _, kind := range []string{"project", "harness"} {
		t.Run(kind, func(t *testing.T) {
			db := startDB(t)
			label := criteriaDestination(t, db, kind, "required")
			issue := routedIssue("1", label)
			issue.Description = "No criteria heading."
			if d, err := start(t, db, issue, 1); err != nil || d != DecisionHeld {
				t.Fatalf("start=%q err=%v", d, err)
			}
			held := readStarted(t, db, "1", 1)
			if kind == "project" {
				exec(t, db, `UPDATE projects SET profile_id='optional-profile' WHERE id='p1'`)
			} else {
				exec(t, db, `UPDATE harnesses SET default_profile_id='optional-profile' WHERE id='fixture-harness'`)
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			d, err := (&TaskStarter{}).RecheckHeldTask(context.Background(), tx, issue)
			if err != nil || d != DecisionStarted {
				t.Fatalf("recheck=%q err=%v", d, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			got := readStarted(t, db, "1", 1)
			if got.id != held.id || got.status != "queued" || got.cause.Valid || got.criteria != "" || got.executions != 1 {
				t.Fatalf("rechecked task=%+v", got)
			}
			// Releasing the hold must preserve the decided destination.
			tasks, err := store.ListTasks(context.Background(), db, store.TaskFilter{}, 0, 10)
			if err != nil || len(tasks) != 1 || kind == "harness" && tasks[0].HarnessID != "fixture-harness" {
				t.Fatalf("tasks=%+v err=%v", tasks, err)
			}
		})
	}
}

func TestMissingDefaultProfilePrecedesAcceptanceCriteria(t *testing.T) {
	db := startDB(t)
	label := criteriaDestination(t, db, "harness", "required")
	exec(t, db, `UPDATE harnesses SET default_profile_id=NULL WHERE id='fixture-harness'`)
	issue := routedIssue("1", label)
	issue.Description = "No criteria heading."
	if d, err := start(t, db, issue, 1); err != nil || d != DecisionHeld {
		t.Fatalf("start=%q err=%v", d, err)
	}
	got := readStarted(t, db, "1", 1)
	if got.status != "blocked" || got.cause.String != "config_missing_profile" || got.executions != 0 || got.criteria != "" {
		t.Fatalf("task=%+v", got)
	}
	if comments := outboxKeys(t, db, got.id); len(comments) != 0 {
		t.Fatalf("missing default wrote comments: %v", comments)
	}
}

// 受け入れ基準 (spec 16 3.2, 9章): required の Profile で
// missing_acceptance_criteria で保留したタスクの Issue に記入依頼のコメントが
// 1回付く; config_missing_profile の保留ではコメントの行ができない. A direct
// special-harness task moves between the two holds on rechecks as its
// default Profile is set and cleared: the config_missing_profile holds never
// write a row, the first missing_acceptance_criteria hold writes the request,
// and returning to it later writes no second row (dedupe_key, 3.5 rule 3).
func TestCriteriaRequestAcrossProfileHolds(t *testing.T) {
	db := startDB(t)
	label := criteriaDestination(t, db, "harness", "required")
	exec(t, db, `UPDATE harnesses SET default_profile_id=NULL WHERE id='fixture-harness'`)
	issue := routedIssue("1", label)
	issue.Description = "No criteria heading."
	if d, err := start(t, db, issue, 1); err != nil || d != DecisionHeld {
		t.Fatalf("start=%q err=%v", d, err)
	}
	held := readStarted(t, db, "1", 1)
	recheck := func(defaultProfile any, wantCause string) {
		t.Helper()
		exec(t, db, `UPDATE harnesses SET default_profile_id=? WHERE id='fixture-harness'`, defaultProfile)
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := (&TaskStarter{}).RecheckHeldTask(context.Background(), tx, issue); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		got := readStarted(t, db, "1", 1)
		if got.id != held.id || got.status != "blocked" || got.cause.String != wantCause || got.executions != 0 {
			t.Fatalf("task=%+v; want blocked/%s", got, wantCause)
		}
	}
	commentRows := func() int {
		t.Helper()
		return countRows(t, db, `SELECT count(*) FROM linear_outbox WHERE task_id = '`+held.id+`' AND action = 'comment'`)
	}

	if held.cause.String != "config_missing_profile" || commentRows() != 0 {
		t.Fatalf("config_missing_profile hold: task=%+v comment rows=%d", held, commentRows())
	}
	recheck("fixture-profile", "missing_acceptance_criteria")
	got := outboxKeys(t, db, held.id)
	if len(got) != 1 || len(got["missing_acceptance_criteria"]) != 1 ||
		got["missing_acceptance_criteria"][0] != held.id+":missing_acceptance_criteria:missing_acceptance_criteria" {
		t.Fatalf("outbox after missing_acceptance_criteria hold = %v", got)
	}
	bodyHas(t, outboxBody(t, db, held.id), "受け入れ条件の記入をお願いします",
		"「受け入れ条件」または「Acceptance Criteria」という見出し")
	recheck(nil, "config_missing_profile")
	recheck("fixture-profile", "missing_acceptance_criteria")
	if n := commentRows(); n != 1 {
		t.Fatalf("comment rows after returning to the criteria hold = %d; want 1", n)
	}
}
