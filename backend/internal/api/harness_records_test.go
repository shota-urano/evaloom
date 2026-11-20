package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

func seedOwnerRecord(t *testing.T, f *authFixture, executionID, taskID, harnessID string, index int) contextRecordSeed {
	t.Helper()
	record := seedContextRecord(t, f, executionID, taskID, harnessID, index)
	id := ulid.Make().String()
	execSQL(t, f, `UPDATE harness_records SET id=? WHERE id=?`, id, record.id)
	record.id = id
	return record
}

func ownerRecords(t *testing.T, f *authFixture, endpoint string) Page[harnessRecordResponse] {
	t.Helper()
	// GET uses session authentication; only state changes require CSRF.
	status, body := f.request(t, "GET", endpoint, true, "", nil)
	var page Page[harnessRecordResponse]
	if status != 200 || json.Unmarshal(body, &page) != nil || page.Items == nil {
		t.Fatalf("list records: %d %s", status, body)
	}
	return page
}

func TestHarnessRecordsListNewestFirstIncludingDeletedAcrossPages(t *testing.T) {
	f, _, _, _ := reportFixture(t)
	sourceID, taskID := contextRecordSource(t, f)
	older := seedOwnerRecord(t, f, sourceID, taskID, "harness-a", 1)
	execSQL(t, f, `UPDATE harness_records SET source_identifier='ENG-123' WHERE id=?`, older.id)
	a := seedOwnerRecord(t, f, sourceID, taskID, "harness-a", 2)
	b := seedOwnerRecord(t, f, sourceID, taskID, "harness-a", 3)
	// Equal timestamps must use an ID tie-break, including across page edges.
	execSQL(t, f, `UPDATE harness_records SET created_at='2026-11-01T00:02:00Z' WHERE id=?`, b.id)
	newest := seedOwnerRecord(t, f, sourceID, taskID, "harness-a", 4)
	// Fractional and whole seconds sort in chronological order.
	execSQL(t, f, `UPDATE harness_records SET created_at='2026-11-01T00:02:00.1Z',
		deleted_at='2026-11-02T00:00:00Z' WHERE id=?`, newest.id)
	execSQL(t, f, `INSERT INTO harnesses(id,name,repo_url,repo_path,created_at,updated_at)
		VALUES ('harness-b','Other','','','2026-11-01T00:00:00Z','2026-11-01T00:00:00Z')`)
	foreign := seedOwnerRecord(t, f, sourceID, taskID, "harness-b", 5)
	want := []string{a.id, b.id}
	sort.Sort(sort.Reverse(sort.StringSlice(want)))
	want = append([]string{newest.id}, append(want, older.id)...)
	endpoint := "/api/harnesses/harness-a/records?limit=1"
	for i, id := range want {
		page := ownerRecords(t, f, endpoint)
		if len(page.Items) != 1 || page.Items[0].ID != id || page.Items[0].HarnessID != "harness-a" {
			t.Fatalf("page %d: %+v; want %s", i, page, id)
		}
		record := page.Items[0]
		if record.ExecutionID != sourceID || record.TaskID != taskID || record.ArtifactID == "" || record.FileName == "" {
			t.Fatalf("missing record metadata: %+v", record)
		}
		if (record.DeletedAt != nil) != (id == newest.id) {
			t.Fatalf("deleted record not distinguished: %+v", record)
		}
		if id == older.id && (record.SourceIdentifier == nil || *record.SourceIdentifier != "ENG-123") {
			t.Fatalf("source identifier missing: %+v", record)
		}
		if (page.NextCursor != "") != (i < len(want)-1) {
			t.Fatalf("wrong next cursor: %+v", page)
		}
		endpoint = "/api/harnesses/harness-a/records?limit=1&cursor=" + url.QueryEscape(page.NextCursor)
	}
	page := ownerRecords(t, f, "/api/harnesses/harness-b/records")
	if len(page.Items) != 1 || page.Items[0].ID != foreign.id || page.NextCursor != "" {
		t.Fatalf("foreign harness: %+v", page)
	}
	execSQL(t, f, `INSERT INTO harnesses(id,name,repo_url,repo_path,created_at,updated_at)
		VALUES ('empty','Empty','','','2026-11-01T00:00:00Z','2026-11-01T00:00:00Z')`)
	if page := ownerRecords(t, f, "/api/harnesses/empty/records"); len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("empty harness: %+v", page)
	}
}

func TestHarnessRecordDeleteExcludesNextWorkAndPreservesListAndArtifacts(t *testing.T) {
	f, token, executionID, _ := reportFixture(t)
	input := contextInput(t, f, executionID)
	setDefinition(t, f, executionID, "$.records", `{"keep":[],"pass":{"recent":1}}`)
	sourceID, taskID := contextRecordSource(t, f)
	older := seedOwnerRecord(t, f, sourceID, taskID, "harness-a", 1)
	newer := seedOwnerRecord(t, f, sourceID, taskID, "harness-a", 2)
	first := ownerRecords(t, f, "/api/harnesses/harness-a/records?limit=1")
	status, body := projectRequest(t, f, "DELETE", "/api/harness-records/"+newer.id, nil)
	if status != 204 || len(body) != 0 {
		t.Fatalf("delete: %d %s", status, body)
	}
	deleted, err := store.GetHarnessRecord(context.Background(), f.db, newer.id)
	if err != nil || deleted.DeletedAt == nil {
		t.Fatalf("record not soft deleted: %+v %v", deleted, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, *deleted.DeletedAt); err != nil {
		t.Fatal(err)
	}
	selected, err := store.HarnessRecordsForNextWork(context.Background(), f.db, "harness-a", 1)
	if err != nil || len(selected) != 1 || selected[0].ID != older.id {
		t.Fatalf("next work selected deleted record: %+v %v", selected, err)
	}
	// Exercise the Runner's context API after the owner's HTTP deletion.
	startStage(t, f, token, executionID, "work")
	status, body = requestContext(t, f, token, executionID, nil, input)
	assertContextRecords(t, f, executionID, status, body, []contextRecordSeed{older})
	page := ownerRecords(t, f, "/api/harnesses/harness-a/records")
	if len(page.Items) != 2 || page.Items[0].ID != newer.id || page.Items[0].DeletedAt == nil || page.Items[1].DeletedAt != nil {
		t.Fatalf("deleted record vanished from list: %+v", page)
	}
	// A cursor issued before deletion remains usable after deletion.
	page = ownerRecords(t, f, "/api/harnesses/harness-a/records?cursor="+url.QueryEscape(first.NextCursor))
	if len(page.Items) != 1 || page.Items[0].ID != older.id {
		t.Fatalf("deletion invalidated cursor: %+v", page)
	}
	var artifactID string
	if err := f.db.QueryRow(`SELECT id FROM artifacts WHERE id=?`, newer.artifactID).Scan(&artifactID); err != nil {
		t.Fatalf("deletion removed source artifact: %v", err)
	}
	// Repeated DELETE keeps the first deletion time and records one change.
	status, body = projectRequest(t, f, "DELETE", "/api/harness-records/"+newer.id, nil)
	if status != 204 {
		t.Fatalf("repeat delete: %d %s", status, body)
	}
	repeated, err := store.GetHarnessRecord(context.Background(), f.db, newer.id)
	if err != nil || !reflect.DeepEqual(deleted, repeated) {
		t.Fatalf("repeat delete changed record: %+v %v", repeated, err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE actor='owner' AND action='harness_record.delete'
		AND target_type='harness_record' AND target_id=? AND json_extract(detail,'$.before.deletedAt') IS NULL
		AND json_extract(detail,'$.after.deletedAt')=?`, newer.id, *deleted.DeletedAt).Scan(&count); err != nil || count != 1 {
		t.Fatalf("delete audit count=%d err=%v", count, err)
	}
}

func TestHarnessRecordsAuthenticationCSRFAndRunnerSeparation(t *testing.T) {
	f, token, _, _ := reportFixture(t)
	sourceID, taskID := contextRecordSource(t, f)
	record := seedOwnerRecord(t, f, sourceID, taskID, "harness-a", 1)
	for _, endpoint := range []struct{ method, path string }{
		{"GET", "/api/harnesses/harness-a/records"},
		{"DELETE", "/api/harness-records/" + record.id},
	} {
		status, body := f.request(t, endpoint.method, endpoint.path, false, f.csrf, nil)
		assertAuthError(t, status, body, 401, Unauthenticated)
		status, body = f.request(t, endpoint.method, endpoint.path, false, "", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
		})
		assertAuthError(t, status, body, 403, Forbidden)
		status, body = f.request(t, endpoint.method, endpoint.path, true, f.csrf, func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
		})
		assertAuthError(t, status, body, 403, Forbidden)
	}
	for _, csrf := range []string{"", "wrong"} {
		status, body := f.request(t, "DELETE", "/api/harness-records/"+record.id, true, csrf, nil)
		assertAuthError(t, status, body, 403, Forbidden)
	}
	stored, err := store.GetHarnessRecord(context.Background(), f.db, record.id)
	if err != nil || stored.DeletedAt != nil {
		t.Fatalf("rejected request changed record: %+v %v", stored, err)
	}
	if page := ownerRecords(t, f, "/api/harnesses/harness-a/records"); len(page.Items) != 1 {
		t.Fatalf("valid session GET: %+v", page)
	}
}

func TestHarnessRecordsNotFoundAndInvalidPagination(t *testing.T) {
	f := newAuthFixture(t)
	for _, endpoint := range []struct{ method, path string }{
		{"GET", "/api/harnesses/missing/records"},
		{"DELETE", "/api/harness-records/" + ulid.Make().String()},
	} {
		status, body := projectRequest(t, f, endpoint.method, endpoint.path, nil)
		assertAuthError(t, status, body, 404, NotFound)
	}
	for _, endpoint := range []struct{ method, path string }{
		{"GET", "/api/harnesses/Invalid_ID/records"},
		{"DELETE", "/api/harness-records/invalid"},
	} {
		status, body := projectRequest(t, f, endpoint.method, endpoint.path, nil)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
	encode := func(value string) string { return url.QueryEscape(base64.RawURLEncoding.EncodeToString([]byte(value))) }
	for _, query := range []string{
		"limit=0", "limit=201", "limit=1&limit=2", "cursor=%%%", "cursor=a&cursor=b", "%=bad",
		"cursor=" + encode("harness-records/harness-b\n2026-11-01T00:00:00Z|"+ulid.Make().String()),
		"cursor=" + encode("harness-records/harness-a\ninvalid|"+ulid.Make().String()),
		"cursor=" + encode("harness-records/harness-a\n2026-11-01T00:00:00Z|invalid"),
	} {
		status, body := projectRequest(t, f, "GET", "/api/harnesses/harness-a/records?"+query, nil)
		assertAuthError(t, status, body, 400, ValidationFailed)
	}
}

func TestHarnessRecordDeleteRollsBackWhenAuditFails(t *testing.T) {
	f, _, _, _ := reportFixture(t)
	sourceID, taskID := contextRecordSource(t, f)
	record := seedOwnerRecord(t, f, sourceID, taskID, "harness-a", 1)
	execSQL(t, f, `CREATE TRIGGER reject_record_audit BEFORE INSERT ON audit_logs
		WHEN NEW.action='harness_record.delete' BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`)
	status, body := projectRequest(t, f, "DELETE", "/api/harness-records/"+record.id, nil)
	assertAuthError(t, status, body, 500, Internal)
	stored, err := store.GetHarnessRecord(context.Background(), f.db, record.id)
	if err != nil || stored.DeletedAt != nil {
		t.Fatalf("failed delete was persisted: %+v %v", stored, err)
	}
	records, err := store.HarnessRecordsForNextWork(context.Background(), f.db, "harness-a", 1)
	if err != nil || len(records) != 1 || records[0].ID != record.id {
		t.Fatalf("failed delete removed next-work input: %+v %v", records, err)
	}
}
