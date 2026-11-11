package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHarnessRecordsForNextWorkExcludesDeletedAndOtherHarnessesBeforeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	db := openRegistry(t, path)
	seedRegistry(t, db)
	seedTaskTables(t, db)
	mustTaskExec(t, db, `INSERT INTO harnesses (id,name,repo_url,repo_path,created_at,updated_at)
		VALUES ('other','Other','','','t','t')`)
	// Both Profile versions share one harness_id; selection has no profile filter.
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("record-%d", i)
		mustTaskExec(t, db, `INSERT INTO artifacts VALUES (?, 'execution-1', NULL, 'check', 'out_file', 'record.md', ?, 'hash', 1, 1, 't')`, id, "artifacts/"+id)
		mustTaskExec(t, db, `INSERT INTO harness_records VALUES (?, 'codex-developer', ?, 'execution-1', 'task-001', NULL, ?, ?, NULL)`, id, id, id+".md", fmt.Sprintf("2026-11-%02dT00:00:00Z", i))
	}
	// The two newest rows must not consume the requested limit.
	mustTaskExec(t, db, `UPDATE harness_records SET deleted_at='2026-11-10T00:00:00Z' WHERE id='record-5'`)
	mustTaskExec(t, db, `UPDATE harness_records SET harness_id='other' WHERE id='record-4'`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	ctx := context.Background()
	for _, test := range []struct {
		harness string
		recent  int
		want    []string
	}{
		{"codex-developer", 2, []string{"record-3", "record-2"}},
		{"codex-developer", 50, []string{"record-3", "record-2", "record-1"}},
		{"codex-developer", 0, []string{}},
		{"other", 50, []string{"record-4"}},
		{"missing", 50, []string{}},
	} {
		records, err := HarnessRecordsForNextWork(ctx, db, test.harness, test.recent)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, record := range records {
			ids = append(ids, record.ID)
			if record.DeletedAt != nil || record.HarnessID != test.harness || record.SourceIdentifier != nil || record.FileName != record.ID+".md" {
				t.Fatal(record)
			}
		}
		if !reflect.DeepEqual(ids, test.want) {
			t.Fatalf("harness=%s recent=%d got=%v want=%v", test.harness, test.recent, ids, test.want)
		}
	}
	for _, recent := range []int{-1, 51} {
		if _, err := HarnessRecordsForNextWork(ctx, db, "codex-developer", recent); err == nil {
			t.Fatal("invalid limit accepted", recent)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE harness_records SET deleted_at='t' WHERE id='record-3'`); err != nil {
		t.Fatal(err)
	}
	records, err := HarnessRecordsForNextWork(ctx, tx, "codex-developer", 1)
	if err != nil || len(records) != 1 || records[0].ID != "record-2" {
		t.Fatal(records, err)
	}
}
