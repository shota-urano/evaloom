package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestClaimReadsWorkConfigurationFromPinnedProfileWithoutLosingNumbers(t *testing.T) {
	db := openRegistry(t, filepath.Join(t.TempDir(), "claim.db"))
	seedRegistry(t, db)
	seedTaskTables(t, db)
	definition := `{"stages":{"work":{"microHarness":"claude_code","toolPermissions":{"limit":9007199254740993}}}}`
	mustTaskExec(t, db, `UPDATE harness_profiles SET definition=? WHERE id='profile-1'`, definition)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	claim, err := NextClaim(context.Background(), tx, "mac-mini")
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil || claim.Profile.Adapter != "claude_code" || claim.Assignment.ProfileID != "profile-1" {
		t.Fatalf("claim=%+v", claim)
	}
	number, ok := claim.Profile.ToolPermissions["limit"].(json.Number)
	if !ok || number.String() != "9007199254740993" {
		t.Fatalf("permission lost precision: %+v", claim.Profile.ToolPermissions)
	}
	if string(claim.Profile.Definition) != definition {
		t.Fatal("claim changed the Profile definition")
	}
}

func TestClaimPinsContractAndParamsAcrossReclaim(t *testing.T) {
	db := openTaskTables(t)
	ctx := context.Background()
	const params = `{"number":9007199254740993}`
	mustTaskExec(t, db, `UPDATE projects SET params=?`, params)
	for attempt := range 2 {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer tx.Rollback()
			claim, err := NextClaim(ctx, tx, "mac-mini")
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v err=%v", claim, err)
			}
			if string(claim.Project.Params) != params {
				t.Fatalf("claim params=%s", claim.Project.Params)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE executions SET status='claimed'`); err != nil {
				t.Fatal(err)
			}
			if err := PinClaim(ctx, tx, claim, "mac-mini", registryTime); err != nil {
				t.Fatal(err)
			}
			var route, project, contract, pinnedParams string
			if err := tx.QueryRowContext(ctx, `SELECT route_kind,project_id,contract_version,params FROM executions`).Scan(&route, &project, &contract, &pinnedParams); err != nil {
				t.Fatal(err)
			}
			if route != "project" || project != "project-a" || contract != "1" || pinnedParams != params {
				t.Fatal(route, project, contract, pinnedParams)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}()
		if attempt == 0 {
			mustTaskExec(t, db, `UPDATE projects SET params='{"changed":true}'; UPDATE executions SET status='pending'; UPDATE tasks SET status='queued'`)
		}
	}
}
