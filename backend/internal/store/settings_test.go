package store

import (
	"context"
	"path/filepath"
	"testing"
)

// The migration creates settings without default rows (spec 02 3.4); a key
// is written once per row and keeps its value across reopening the DB.
func TestSettingsTableStartsEmptyAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	db := openRegistry(t, path)
	ctx := context.Background()
	if rows, err := ReadSettings(ctx, db); err != nil || len(rows) != 0 {
		t.Fatalf("rows = %v err=%v; want no default rows", rows, err)
	}
	for _, value := range []string{`"go"`, `"go2"`} {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := PutSetting(ctx, tx, "linear.triggerLabel", value); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	db = openRegistry(t, path)
	rows, err := ReadSettings(ctx, db)
	if err != nil || len(rows) != 1 || rows["linear.triggerLabel"] != `"go2"` {
		t.Fatalf("rows after reopen = %v err=%v", rows, err)
	}
}
