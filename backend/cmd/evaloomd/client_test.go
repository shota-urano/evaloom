package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

func clientTestDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evaloom.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO harnesses (id, name, repo_url, repo_path, created_at, updated_at)
		VALUES ('researcher', 'researcher', 'file:///r', '/r', '2026-11-28T00:00:00Z', '2026-11-28T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	return path
}

// spec 20 4.3 and 9章: the key appears only in register's output, the DB keeps
// the hash, list never shows either, and revoke is recorded.
func TestClientRegisterListRevoke(t *testing.T) {
	ctx := context.Background()
	path := clientTestDB(t)
	var out bytes.Buffer
	if err := run(ctx, []string{"-db", path, "client", "register", "-harness", "researcher", "-name", "brain research", "brain-research"}, &out); err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSuffix(out.String(), "\n")
	if secret, err := base64.RawURLEncoding.DecodeString(key); err != nil || len(secret) != 32 || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("register must print one 256-bit key once: %q", out.String())
	}
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var hash, harness, name string
	if err := db.QueryRow(`SELECT token_hash, harness_id, name FROM program_clients WHERE id='brain-research'`).Scan(&hash, &harness, &name); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(key))
	if hash != hex.EncodeToString(sum[:]) || harness != "researcher" || name != "brain research" {
		t.Fatalf("stored hash=%s harness=%s name=%s", hash, harness, name)
	}
	var dump string
	if err := db.QueryRow(`SELECT group_concat(actor || action || target_id || detail, '|') FROM audit_logs`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, key) || strings.Contains(dump, hash) || !strings.Contains(dump, "program_client.register") {
		t.Fatalf("audit must record registration without the key: %s", dump)
	}

	out.Reset()
	if err := run(ctx, []string{"-db", path, "client", "list"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "brain-research\tbrain research\tharness:researcher") || strings.Contains(out.String(), key) || strings.Contains(out.String(), hash) {
		t.Fatalf("list=%q", out.String())
	}
	out.Reset()
	if err := run(ctx, []string{"-db", path, "client", "revoke", "brain-research"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProgramClientByTokenHash(ctx, db, hash); err == nil {
		t.Fatal("revoked key still resolves")
	}
	var revokes int
	if err := db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action='program_client.revoke'`).Scan(&revokes); err != nil || revokes != 1 {
		t.Fatalf("revoke audits=%d err=%v", revokes, err)
	}
}

func TestClientRegisterRejectsBadTargets(t *testing.T) {
	ctx := context.Background()
	path := clientTestDB(t)
	for name, args := range map[string][]string{
		"no target":      {"client", "register", "c1"},
		"both targets":   {"client", "register", "-harness", "researcher", "-project", "p", "c1"},
		"unknown target": {"client", "register", "-harness", "missing", "c1"},
		"bad id":         {"client", "register", "-harness", "researcher", "a/b"},
		"unknown verb":   {"client", "rotate", "c1"},
	} {
		var out bytes.Buffer
		if err := run(ctx, append([]string{"-db", path}, args...), &out); err == nil {
			t.Errorf("%s: want error, output=%q", name, out.String())
		} else if out.Len() != 0 && strings.Count(out.String(), "\n") == 1 && len(strings.TrimSpace(out.String())) == 43 {
			t.Errorf("%s: printed a key", name)
		}
	}
	var out bytes.Buffer
	if err := run(ctx, []string{"-db", path, "client", "register", "-harness", "researcher", "c1"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"-db", path, "client", "register", "-harness", "researcher", "c1"}, &bytes.Buffer{}); err == nil {
		t.Fatal("duplicate client ID must fail")
	}
}
