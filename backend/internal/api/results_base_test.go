package api

import (
	"database/sql"
	"strings"
	"testing"
)

func TestBaseResultPersistsImmutableSHAWithoutTransition(t *testing.T) {
	f, token, id, _ := resultFixture(t, "preparing")
	before := readResultSnapshot(t, f, id)
	sha := strings.Repeat("b", 40)
	input := map[string]any{"fromStatus": "preparing", "status": "preparing", "baseSha": sha}
	for i := 0; i < 2; i++ {
		status, body := postResult(t, f, token, id, input)
		if status != 204 {
			t.Fatalf("record/replay SHA: %d %s", status, body)
		}
		var saved string
		if err := f.db.QueryRow(`SELECT base_sha FROM executions WHERE id=?`, id).Scan(&saved); err != nil || saved != sha {
			t.Fatalf("saved SHA=%s err=%v", saved, err)
		}
		if after := readResultSnapshot(t, f, id); after != before {
			t.Fatalf("SHA-only report replayed transition side effects: %+v -> %+v", before, after)
		}
	}
	input["baseSha"] = strings.Repeat("c", 40)
	status, body := postResult(t, f, token, id, input)
	assertAuthError(t, status, body, 422, PreconditionFailed)
	var saved string
	if err := f.db.QueryRow(`SELECT base_sha FROM executions WHERE id=?`, id).Scan(&saved); err != nil || saved != sha {
		t.Fatalf("immutable SHA changed: %s %v", saved, err)
	}
}

func TestBaseResultRejectsInvalidOrBundledMeasurementsWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"empty", "baseSha", ""}, {"abbreviated", "baseSha", "abcd"},
		{"worktree", "worktreeId", "untrusted"}, {"branch", "branchName", "untrusted"},
		{"error", "errorMessage", "secret"}, {"usage", "usage", map[string]any{"wallTimeMs": 1}},
		{"gate", "gateResults", []map[string]any{{"gate": 3, "result": "pass"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, "preparing")
			before := readResultSnapshot(t, f, id)
			input := map[string]any{"fromStatus": "preparing", "status": "preparing", "baseSha": strings.Repeat("a", 40)}
			input[tc.field] = tc.value
			status, body := postResult(t, f, token, id, input)
			assertAuthError(t, status, body, 400, ValidationFailed)
			var saved sql.NullString
			if err := f.db.QueryRow(`SELECT base_sha FROM executions WHERE id=?`, id).Scan(&saved); err != nil || saved.Valid {
				t.Fatalf("invalid measurement saved: %+v %v", saved, err)
			}
			if after := readResultSnapshot(t, f, id); after != before {
				t.Fatalf("rejected measurement mutated DB: %+v -> %+v", before, after)
			}
		})
	}
}

func TestBaseResultRequiresPreparingAndOwningRunner(t *testing.T) {
	for _, state := range []string{"claimed", "running", "verifying", "publishing"} {
		t.Run(state, func(t *testing.T) {
			f, token, id, _ := resultFixture(t, state)
			status, body := postResult(t, f, token, id, map[string]any{
				"fromStatus": state, "status": state, "baseSha": strings.Repeat("a", 40),
			})
			assertAuthError(t, status, body, 409, Conflict)
		})
	}
	f, token, id, _ := resultFixture(t, "preparing")
	if _, err := f.db.Exec(`UPDATE executions SET runner_id=NULL WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	status, body := postResult(t, f, token, id, map[string]any{
		"fromStatus": "preparing", "status": "preparing", "baseSha": strings.Repeat("a", 40),
	})
	assertAuthError(t, status, body, 403, Forbidden)
}
