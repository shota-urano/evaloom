package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/store"
	"golang.org/x/crypto/argon2"
)

// Acceptance: CLI saves only ID + salted hash, records setting/resetting in
// audit_logs, and revokes every session even when the login ID changes.
func TestOwnerCLIStoresHashAuditsAndRevokesAllSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.db")
	passwords := []string{"  first owner password  ", "replacement password", "replacement password"}
	ids := []string{"owner", "renamed-owner", "renamed-owner"}
	var previousHash, createdAt string
	for i, password := range passwords {
		output, err := runOwnerCommand(t, path, []string{"set-password", "-id", ids[i]}, password+"\n"+password+"\n")
		if err != nil {
			t.Fatalf("set-password: %v", err)
		}
		if strings.Contains(output, password) || strings.Contains(output, "$argon2") || strings.Contains(output, "login URL") {
			t.Fatal("CLI output contains a password, hash or login URL")
		}
		db, err := store.Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		got := readOwnerCredential(t, db)
		if got.ID != ids[i] || countOwnerRows(t, db, "owner_credentials") != 1 {
			t.Fatal("CLI did not replace the single owner ID")
		}
		assertOwnerPasswordHash(t, got.Hash, password)
		if got.Hash == previousHash {
			t.Fatal("password reset reused the previous salted hash")
		}
		previousHash = got.Hash
		if i == 0 {
			// A fixed old timestamp makes creation-time preservation observable
			// without relying on sleeps or clock precision.
			createdAt = "2020-01-01T00:00:00Z"
			if _, err := db.Exec(`UPDATE owner_credentials SET created_at = ?, updated_at = ?`, createdAt, createdAt); err != nil {
				t.Fatal(err)
			}
		} else if got.CreatedAt != createdAt || got.UpdatedAt == createdAt {
			t.Fatal("reset did not preserve created_at and replace updated_at")
		}
		for _, timestamp := range []string{got.CreatedAt, got.UpdatedAt} {
			if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil || !strings.HasSuffix(timestamp, "Z") {
				t.Fatalf("owner timestamp is not UTC RFC3339: %v", err)
			}
		}
		if countOwnerRows(t, db, "owner_sessions") != 0 {
			t.Fatal("password reset left owner sessions active")
		}
		if countOwnerRows(t, db, "audit_logs") != i+1 {
			t.Fatal("each successful setting must produce one audit record")
		}
		var actor, action, targetType, targetID, detail, at string
		if err := db.QueryRow(`SELECT actor, action, target_type, target_id, detail, created_at FROM audit_logs ORDER BY id DESC LIMIT 1`).Scan(
			&actor, &action, &targetType, &targetID, &detail, &at); err != nil {
			t.Fatal(err)
		}
		if actor != "owner" || action != "owner.set_password" || targetType != "owner_credentials" || targetID != ids[i] || !json.Valid([]byte(detail)) {
			t.Fatal("audit does not identify the owner password setting")
		}
		if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
			t.Fatal("audit is missing a valid timestamp")
		}
		audit := strings.Join([]string{actor, action, targetType, targetID, detail, at}, " ")
		for _, secret := range []string{password, got.Hash, strings.Split(got.Hash, "$")[5]} {
			if strings.Contains(audit, secret) {
				t.Fatal("audit contains a password or derived hash")
			}
		}
		if i < len(passwords)-1 {
			for n, via := range []string{"loopback", "tailnet", "tailnet"} {
				if _, err := db.Exec(`INSERT INTO owner_sessions VALUES (?, ?, ?, ?, ?, ?)`,
					fmt.Sprintf("session-%d", n), fmt.Sprintf("token-hash-%d", n), "csrf-hash", at, at, via); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		// Check SQLite's persisted bytes too, including a WAL if still present.
		for _, suffix := range []string{"", "-wal", "-shm"} {
			data, err := os.ReadFile(path + suffix)
			if os.IsNotExist(err) && suffix != "" {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range passwords {
				if bytes.Contains(data, []byte(secret)) {
					t.Fatal("SQLite files contain a plaintext password")
				}
			}
		}
	}
}

// Acceptance: a mismatch, incomplete input or invalid configuration must not
// create a database, change credentials, delete sessions or write an audit.
func TestOwnerCLIRejectsInputWithoutSaving(t *testing.T) {
	const password = "original owner password"
	cases := []struct {
		name, input string
		args        []string
	}{
		{"mismatch", password + "\nother owner password\n", []string{"set-password", "-id", "new-owner"}},
		{"missing confirmation", password + "\n", []string{"set-password", "-id", "new-owner"}},
		{"empty input", "", []string{"set-password", "-id", "new-owner"}},
		{"empty password", "\n\n", []string{"set-password", "-id", "new-owner"}},
		{"short password", "short password\nshort password\n", []string{"set-password", "-id", "new-owner"}},
		{"invalid UTF8", strings.Repeat("a", 15) + "\xff\n" + strings.Repeat("a", 15) + "\xff\n", []string{"set-password", "-id", "new-owner"}},
		{"missing id", "", []string{"set-password"}},
		{"blank id", "", []string{"set-password", "-id", "  "}},
		{"control in id", "", []string{"set-password", "-id", "owner\n"}},
		{"extra argument", "", []string{"set-password", "-id", "owner", "unexpected"}},
		{"unknown command", "", []string{"unknown"}},
	}
	for _, existing := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("existing=%t/%s", existing, tc.name), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "owner.db")
				var before ownerCredential
				if existing {
					db, err := store.Open(context.Background(), path)
					if err != nil {
						t.Fatal(err)
					}
					if err := store.SetOwnerCredentials(context.Background(), db, "original-owner", "unchanged-hash"); err != nil {
						t.Fatal(err)
					}
					if _, err := db.Exec(`INSERT INTO owner_sessions VALUES ('session', 'hash', 'csrf', 'now', 'later', 'tailnet')`); err != nil {
						t.Fatal(err)
					}
					before = readOwnerCredential(t, db)
					db.Close()
				}
				output, err := runOwnerCommand(t, path, tc.args, tc.input)
				if err == nil {
					t.Fatal("invalid command/input was accepted")
				}
				if strings.Contains(output+err.Error(), password) || strings.Contains(output+err.Error(), "unchanged-hash") {
					t.Fatal("failure output leaked credentials")
				}
				if !existing {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("invalid input created a DB: %v", err)
					}
					return
				}
				db, err := store.Open(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if readOwnerCredential(t, db) != before || countOwnerRows(t, db, "owner_sessions") != 1 || countOwnerRows(t, db, "audit_logs") != 1 {
					t.Fatal("invalid input changed credentials, sessions or audit logs")
				}
			})
		}
	}
}

func TestOwnerCLIAcceptsPipeCRLFAndFinalLineWithoutNewline(t *testing.T) {
	const password = "日本語と spaces を含むパスワード "
	path := filepath.Join(t.TempDir(), "owner.db")
	if _, err := runOwnerCommand(t, path, []string{"set-password", "-id", "owner"}, password+"\r\n"+password); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertOwnerPasswordHash(t, readOwnerCredential(t, db).Hash, password)
}

func runOwnerCommand(t *testing.T, path string, args []string, input string) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := writer.WriteString(input); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	writer.Close()
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = runWithInput(ctx, append([]string{"-db", path, "owner"}, args...), reader, &output)
	return output.String(), err
}

type ownerCredential struct{ ID, Hash, CreatedAt, UpdatedAt string }

func readOwnerCredential(t *testing.T, db *sql.DB) ownerCredential {
	t.Helper()
	var c ownerCredential
	if err := db.QueryRow(`SELECT id, password_hash, created_at, updated_at FROM owner_credentials`).Scan(&c.ID, &c.Hash, &c.CreatedAt, &c.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	return c
}

func countOwnerRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertOwnerPasswordHash(t *testing.T, hash, password string) {
	t.Helper()
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=4" {
		t.Fatal("CLI did not persist a self-contained Argon2id hash")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		t.Fatal("invalid password salt")
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || !bytes.Equal(key, argon2.IDKey([]byte(password), salt, 3, 65536, 4, 32)) {
		t.Fatal("stored hash does not match the CLI password")
	}
}
