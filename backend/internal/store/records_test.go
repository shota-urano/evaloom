package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"modernc.org/sqlite/lib"
)

func TestVerificationMigrationUpgradesAndRoundTripsAllTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaloom.db")
	previous := fstest.MapFS{}
	for _, name := range []string{"0001_registry.sql", "0002_tasks.sql"} {
		data, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		previous[name] = &fstest.MapFile{Data: data}
	}
	db, err := open(context.Background(), path, previous)
	if err != nil {
		t.Fatal(err)
	}
	seedRegistry(t, db)
	seedTaskTables(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	seedVerificationRecords(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openRegistry(t, path)
	assertVersions(t, db, []int64{1, 2, 3, 4, 5, 6, 7})
	sha := strings.Repeat("a", 40)
	for _, test := range []struct {
		table string
		want  []any
	}{
		{"artifacts", []any{"artifact-1", "execution-1", "test_ids", "artifacts/test-ids.json", strings.Repeat("b", 64), int64(12), int64(1), registryTime}},
		{"test_runs", []any{"test-1", "execution-1", "verify", sha, "go test ./...", int64(0), int64(4), int64(2), int64(1), int64(1), "artifact-1", int64(0), registryTime, registryTime}},
		{"gate_results", []any{"gate-1", "execution-1", int64(1), "blocked", sha, sha, `[{"rule":"existing_test_deleted","path":"foo_test.go","line":42,"message":"test deleted"}]`, registryTime}},
		{"execution_blocks", []any{"block-1", "execution-1", "gate1_test_change", "gate-1", sha, `{"reason":"test deleted"}`, nil, nil, registryTime}},
		{"audit_logs", []any{int64(1), "owner", "project.update", "project", "project-a", `{"before":"main","after":"develop"}`, registryTime}},
		{"usage_records", []any{"usage-1", "execution-1", int64(100), int64(20), int64(1500), float64(0.125), registryTime}},
	} {
		t.Run(test.table, func(t *testing.T) {
			got := make([]any, len(test.want))
			dest := make([]any, len(got))
			for i := range got {
				dest[i] = &got[i]
			}
			if err := db.QueryRow("SELECT * FROM " + test.table).Scan(dest...); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("persisted row = %#v; want %#v", got, test.want)
			}
		})
	}
	var criteria string
	if err := db.QueryRow("SELECT acceptance_criteria FROM tasks WHERE id = 'task-001'").Scan(&criteria); err != nil || criteria != "tests pass" {
		t.Fatalf("pre-existing task = %q, err = %v", criteria, err)
	}
}

func TestVerificationRecordsPreserveUnknownMeasurementsAsNull(t *testing.T) {
	db := openTaskTables(t)
	mustTaskExec(t, db, `INSERT INTO test_runs
		(id, execution_id, kind, target_sha, command, timed_out, started_at, finished_at)
		VALUES ('setup-1', 'execution-1', 'setup', ?, 'make setup', 1, ?, ?)`, strings.Repeat("a", 40), registryTime, registryTime)
	var exit, total, passed, failed, skipped sql.NullInt64
	var artifact sql.NullString
	if err := db.QueryRow(`SELECT exit_code, total, passed, failed, skipped, test_ids_artifact_id FROM test_runs`).Scan(
		&exit, &total, &passed, &failed, &skipped, &artifact); err != nil {
		t.Fatal(err)
	}
	if exit.Valid || total.Valid || passed.Valid || failed.Valid || skipped.Valid || artifact.Valid {
		t.Fatal("unavailable test measurements must remain NULL")
	}
	mustTaskExec(t, db, `INSERT INTO usage_records (id, execution_id, wall_time_ms, created_at)
		VALUES ('usage-1', 'execution-1', 1500, ?)`, registryTime)
	var input, output sql.NullInt64
	var cost sql.NullFloat64
	if err := db.QueryRow("SELECT input_tokens, output_tokens, cost_amount FROM usage_records").Scan(&input, &output, &cost); err != nil {
		t.Fatal(err)
	}
	if input.Valid || output.Valid || cost.Valid {
		t.Fatal("unavailable token counts and non-API cost must remain NULL")
	}
	if err := RecordExecutionBlock(context.Background(), db, ExecutionBlock{
		ID: "context-block", ExecutionID: "execution-1", Cause: "context_conflict", HeadSHA: strings.Repeat("a", 40),
	}); err != nil {
		t.Fatal(err)
	}
	var gate, decision, decidedAt sql.NullString
	if err := db.QueryRow("SELECT gate_result_id, decision, decided_at FROM execution_blocks").Scan(&gate, &decision, &decidedAt); err != nil {
		t.Fatal(err)
	}
	if gate.Valid || decision.Valid || decidedAt.Valid {
		t.Fatal("non-gate block must preserve NULL reference and pending decision")
	}
}

func TestVerificationRecordReferences(t *testing.T) {
	db := openTaskTables(t)
	seedVerificationRecords(t, db)
	for _, test := range []struct{ table, column string }{
		{"artifacts", "execution_id"}, {"test_runs", "execution_id"}, {"test_runs", "test_ids_artifact_id"},
		{"gate_results", "execution_id"}, {"execution_blocks", "execution_id"},
		{"execution_blocks", "gate_result_id"}, {"usage_records", "execution_id"},
	} {
		t.Run(test.table+"/"+test.column, func(t *testing.T) {
			_, err := db.Exec("UPDATE " + test.table + " SET " + test.column + " = 'missing'")
			assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
		})
	}
	for _, table := range []string{"executions", "artifacts", "gate_results"} {
		_, err := db.Exec("DELETE FROM " + table)
		assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
	}
}

func TestVerificationRecordEnums(t *testing.T) {
	db := openTaskTables(t)
	seedVerificationRecords(t, db)
	for _, test := range []struct {
		table, column string
		allowed       []any
		invalid       []any
	}{
		{"artifacts", "kind", []any{"harness_log", "test_log", "setup_log", "diff", "test_report", "test_ids", "harness_output"}, []any{"unknown", ""}},
		{"artifacts", "redacted", []any{0, 1}, []any{-1, 2}},
		{"test_runs", "kind", []any{"setup", "baseline", "verify"}, []any{"unknown", ""}},
		{"test_runs", "timed_out", []any{0, 1}, []any{-1, 2}},
		{"gate_results", "gate", []any{1, 2, 3}, []any{0, 4}},
		{"gate_results", "result", []any{"pass", "blocked", "failed"}, []any{"unknown", "passed"}},
		{"execution_blocks", "cause", []any{"gate1_test_change", "gate1_unverifiable", "gate2_missing_tests", "gate2_unverifiable", "gate3_mismatch", "context_conflict", "git_state"}, []any{"unknown", ""}},
		{"execution_blocks", "decision", []any{nil, "continue", "abort"}, []any{"unknown", ""}},
		{"audit_logs", "actor", []any{"owner", "system", "runner:mac-mini"}, []any{"unknown", "runner:", ""}},
	} {
		t.Run(test.table+"/"+test.column, func(t *testing.T) {
			query := "UPDATE " + test.table + " SET " + test.column + " = ?"
			for _, value := range test.allowed {
				mustTaskExec(t, db, query, value)
			}
			for _, value := range test.invalid {
				_, err := db.Exec(query, value)
				assertRegistryConstraint(t, err, sqlite3.SQLITE_CONSTRAINT_CHECK)
			}
		})
	}
}

func TestRecordWritersSerializeJSONBeforeInserting(t *testing.T) {
	db := openTaskTables(t)
	ctx := context.Background()
	finding := Finding{Rule: "existing_test_deleted", Path: "foo_test.go", Line: 42, Message: "test deleted"}
	if err := RecordGateResult(ctx, db, GateResult{
		ID: "gate-1", ExecutionID: "execution-1", Gate: 1, Result: "blocked",
		BaseSHA: "base", TargetSHA: "target", Findings: []Finding{finding},
	}); err != nil {
		t.Fatal(err)
	}
	var encoded, createdAt string
	if err := db.QueryRow("SELECT findings, created_at FROM gate_results").Scan(&encoded, &createdAt); err != nil {
		t.Fatal(err)
	}
	var findings []Finding
	if err := json.Unmarshal([]byte(encoded), &findings); err != nil || !reflect.DeepEqual(findings, []Finding{finding}) {
		t.Fatalf("findings = %#v, err = %v", findings, err)
	}
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil || !strings.HasSuffix(createdAt, "Z") {
		t.Fatalf("created_at = %q, err = %v; want UTC RFC3339", createdAt, err)
	}
	if err := RecordGateResult(ctx, db, GateResult{ID: "gate-2", ExecutionID: "execution-1", Gate: 2, Result: "pass"}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT findings FROM gate_results WHERE id = 'gate-2'").Scan(&encoded); err != nil || encoded != "[]" {
		t.Fatalf("empty findings = %q, err = %v; want []", encoded, err)
	}
	gate := "gate-1"
	if err := RecordExecutionBlock(ctx, db, ExecutionBlock{
		ID: "block-1", ExecutionID: "execution-1", Cause: "gate1_test_change", GateResultID: &gate,
		HeadSHA: "target", Detail: map[string]any{"path": "foo_test.go"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT detail FROM execution_blocks").Scan(&encoded); err != nil || encoded != `{"path":"foo_test.go"}` {
		t.Fatalf("block detail = %q, err = %v", encoded, err)
	}
	id, err := RecordAuditLog(ctx, db, AuditLog{Actor: "system", Action: "auth.reject", TargetType: "request", TargetID: "request-1"})
	if err != nil || id <= 0 {
		t.Fatalf("audit ID = %d, err = %v", id, err)
	}
	if err := db.QueryRow("SELECT detail FROM audit_logs WHERE id = ?", id).Scan(&encoded); err != nil || encoded != "{}" {
		t.Fatalf("empty detail = %q, err = %v; want {}", encoded, err)
	}
	mustTaskExec(t, db, "DELETE FROM audit_logs WHERE id = ?", id)
	next, err := RecordAuditLog(ctx, db, AuditLog{Actor: "runner:mac-mini", Action: "result", TargetType: "execution", TargetID: "execution-1"})
	if err != nil || next <= id {
		t.Fatalf("audit ID after deletion = %d, err = %v; want > %d", next, err, id)
	}
	// JSON encoding failures must not leave partial records.
	for _, detail := range []map[string]any{
		{"count": math.NaN()}, {"invalid": json.RawMessage(`{"unterminated":`)}, {"callback": func() {}},
	} {
		if err := RecordExecutionBlock(ctx, db, ExecutionBlock{
			ID: "invalid", ExecutionID: "execution-1", Cause: "git_state", Detail: detail,
		}); err == nil {
			t.Fatal("invalid detail was accepted for block")
		}
		if _, err := RecordAuditLog(ctx, db, AuditLog{Actor: "owner", Action: "update", Detail: detail}); err == nil {
			t.Fatal("invalid detail was accepted for audit log")
		}
	}
	for _, table := range []string{"execution_blocks", "audit_logs"} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s rows after rejected writes = %d, err = %v; want 1", table, count, err)
		}
	}
}

func seedVerificationRecords(t *testing.T, db *sql.DB) {
	t.Helper()
	sha := strings.Repeat("a", 40)
	mustTaskExec(t, db, `INSERT INTO artifacts (id, execution_id, kind, storage_path, sha256, size_bytes, redacted, created_at)
		VALUES ('artifact-1', 'execution-1', 'test_ids', 'artifacts/test-ids.json', ?, 12, 1, ?)`, strings.Repeat("b", 64), registryTime)
	mustTaskExec(t, db, `INSERT INTO test_runs
		(id, execution_id, kind, target_sha, command, exit_code, total, passed, failed, skipped, test_ids_artifact_id, timed_out, started_at, finished_at)
		VALUES ('test-1', 'execution-1', 'verify', ?, 'go test ./...', 0, 4, 2, 1, 1, 'artifact-1', 0, ?, ?)`, sha, registryTime, registryTime)
	mustTaskExec(t, db, `INSERT INTO gate_results (id, execution_id, gate, result, base_sha, target_sha, findings, created_at)
		VALUES ('gate-1', 'execution-1', 1, 'blocked', ?, ?, ?, ?)`, sha, sha,
		`[{"rule":"existing_test_deleted","path":"foo_test.go","line":42,"message":"test deleted"}]`, registryTime)
	mustTaskExec(t, db, `INSERT INTO execution_blocks (id, execution_id, cause, gate_result_id, head_sha, detail, created_at)
		VALUES ('block-1', 'execution-1', 'gate1_test_change', 'gate-1', ?, '{"reason":"test deleted"}', ?)`, sha, registryTime)
	mustTaskExec(t, db, `INSERT INTO audit_logs (actor, action, target_type, target_id, detail, created_at)
		VALUES ('owner', 'project.update', 'project', 'project-a', '{"before":"main","after":"develop"}', ?)`, registryTime)
	mustTaskExec(t, db, `INSERT INTO usage_records (id, execution_id, input_tokens, output_tokens, wall_time_ms, cost_amount, created_at)
		VALUES ('usage-1', 'execution-1', 100, 20, 1500, 0.125, ?)`, registryTime)
}
