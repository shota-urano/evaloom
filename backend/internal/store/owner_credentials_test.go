package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSetOwnerCredentialsRollsBackEveryWriteOnFailure(t *testing.T) {
	for _, initial := range []bool{true, false} {
		for _, failure := range []string{"credentials", "sessions", "audit"} {
			t.Run(map[bool]string{true: "initial", false: "reset"}[initial]+"/"+failure, func(t *testing.T) {
				db := openRegistry(t, filepath.Join(t.TempDir(), "owner.db"))
				if !initial {
					if err := SetOwnerCredentials(context.Background(), db, "old-owner", "old-hash"); err != nil {
						t.Fatal(err)
					}
				}
				mustTaskExec(t, db, `INSERT INTO owner_sessions (id, token_hash, csrf_token_hash, created_at, expires_at, via)
					VALUES ('session', 'token-hash', 'csrf-hash', ?, ?, 'loopback')`, registryTime, registryTime)
				beforeCredentials := migrationRows(t, db, `SELECT * FROM owner_credentials`)
				beforeSessions := migrationRows(t, db, `SELECT * FROM owner_sessions`)
				beforeAudit := migrationRows(t, db, `SELECT * FROM audit_logs`)
				trigger := map[string]string{
					"credentials": `CREATE TRIGGER reject_credentials BEFORE ` + map[bool]string{true: "INSERT", false: "UPDATE"}[initial] + ` ON owner_credentials BEGIN SELECT RAISE(ABORT, 'write failed'); END`,
					"sessions":    `CREATE TRIGGER reject_sessions BEFORE DELETE ON owner_sessions BEGIN SELECT RAISE(ABORT, 'write failed'); END`,
					"audit":       `CREATE TRIGGER reject_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT, 'write failed'); END`,
				}[failure]
				mustTaskExec(t, db, trigger)
				if err := SetOwnerCredentials(context.Background(), db, "new-owner", "new-hash"); err == nil {
					t.Fatal("write failure was accepted")
				}
				for query, want := range map[string][][]any{
					`SELECT * FROM owner_credentials`: beforeCredentials,
					`SELECT * FROM owner_sessions`:    beforeSessions,
					`SELECT * FROM audit_logs`:        beforeAudit,
				} {
					if got := migrationRows(t, db, query); !reflect.DeepEqual(got, want) {
						t.Fatalf("failed transaction changed %s: got %#v, want %#v", query, got, want)
					}
				}
			})
		}
	}
}
