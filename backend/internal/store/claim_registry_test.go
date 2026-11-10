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
