package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
)

// spec 17 3.2 (Q60): 0020 turns every Phase 1 approval_policy that is not the
// Phase 2 shape into {"version":1,"mergePullRequest":"require_approval"} and
// keeps values already in that shape, including direct_push.
func TestApprovalPolicyMigrationDefaultsExistingProjectsToRequireApproval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := readMigrations(source)
	if err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	for _, migration := range migrations {
		if migration.version < 20 {
			previous[migration.name] = &fstest.MapFile{Data: []byte(migration.sql)}
		}
	}
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	const defaultPolicy = `{"mergePullRequest":"require_approval","version":1}`
	stored := []struct{ policy, want string }{
		{`{}`, defaultPolicy},
		{`{"publish":"ask"}`, defaultPolicy},
		{`{"version":1}`, defaultPolicy},
		{`{"mergePullRequest":"direct_push"}`, defaultPolicy},
		{`{"version":2,"mergePullRequest":"direct_push"}`, defaultPolicy},
		{`{"version":"1","mergePullRequest":"direct_push"}`, defaultPolicy},
		{`{"version":1.5,"mergePullRequest":"direct_push"}`, defaultPolicy},
		{`{"version":1,"mergePullRequest":"auto_merge"}`, defaultPolicy},
		{`{"version":1,"mergePullRequest":true}`, defaultPolicy},
		{`{"version":1,"mergePullRequest":"direct_push","deployProduction":"direct_push"}`, defaultPolicy},
		{`[]`, defaultPolicy},
		{`"require_approval"`, defaultPolicy},
		{`not json`, defaultPolicy},
		{`{"version":1,"mergePullRequest":"direct_push"}`, `{"version":1,"mergePullRequest":"direct_push"}`},
		{`{"mergePullRequest":"require_approval","version":1}`, defaultPolicy},
	}
	ids := make([]string, len(stored))
	for i, row := range stored {
		ids[i] = "project-" + string(rune('b'+i))
		insertRegistryProject(t, db, ids[i], "/repos/"+ids[i])
		if _, err := db.Exec(`UPDATE projects SET approval_policy = ? WHERE id = ?`, row.policy, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21})
	for i, row := range stored {
		got := migrationRows(t, db, `SELECT approval_policy FROM projects WHERE id = '`+ids[i]+`'`)
		if !reflect.DeepEqual(got, [][]any{{row.want}}) {
			t.Errorf("approval_policy %s migrated to %#v, want %s", row.policy, got, row.want)
		}
	}
	// The seeded Project already had the Phase 2 shape and is read back as it.
	p, err := GetProject(context.Background(), db, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if p.ApprovalPolicy["mergePullRequest"] != "require_approval" || len(p.ApprovalPolicy) != 2 {
		t.Fatalf("ApprovalPolicy = %#v", p.ApprovalPolicy)
	}
}
