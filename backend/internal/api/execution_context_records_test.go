package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

type contextRecordSeed struct {
	id, fileName, artifactID, hash string
}

// Use a completed Execution on another Project and Profile version: records
// belong to the Harness, not to a Project or one Profile version.
func contextRecordSource(t *testing.T, f *authFixture) (string, string) {
	t.Helper()
	task := createClaimTask(t, f, "project-b", "record-source")
	var id string
	if err := f.db.QueryRow(`SELECT id FROM executions WHERE task_id=?`, task.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	execSQL(t, f, `UPDATE executions SET status='succeeded',harness_id='harness-a',
		profile_id=(SELECT id FROM harness_profiles WHERE harness_id='harness-a' AND version='2.0.0'),
		profile_version='2.0.0' WHERE id=?`, id)
	execSQL(t, f, `UPDATE tasks SET status='completed' WHERE id=?`, task.ID)
	return id, task.ID
}

func seedContextRecord(t *testing.T, f *authFixture, executionID, taskID, harnessID string, index int) contextRecordSeed {
	t.Helper()
	id := fmt.Sprintf("record-%03d", index)
	// Lexical filenames run oldest first, unlike the required response order.
	name := fmt.Sprintf("2026-11-01-record-%03d.md", index)
	artifactID := addArtifact(t, f, executionID, "check", "out_file", "report.md")
	hash := contextHash([]byte(id))
	execSQL(t, f, `UPDATE artifacts SET sha256=? WHERE id=?`, hash, artifactID)
	execSQL(t, f, `INSERT INTO harness_records
		(id,harness_id,artifact_id,execution_id,task_id,file_name,created_at)
		VALUES (?,?,?,?,?,?,?)`, id, harnessID, artifactID, executionID, taskID, name,
		fmt.Sprintf("2026-11-01T00:%02d:00Z", index))
	return contextRecordSeed{id, name, artifactID, hash}
}

func assertContextRecords(t *testing.T, f *authFixture, id string, status int, body []byte, want []contextRecordSeed) {
	t.Helper()
	var reply executionContextReply
	if status != 200 || json.Unmarshal(body, &reply) != nil {
		t.Fatalf("context: %d %s", status, body)
	}
	wantRecords := []contextRecord{}
	wantPins := map[string]contextRecordSeed{}
	for _, record := range want {
		wantRecords = append(wantRecords, contextRecord{record.fileName, record.artifactID})
		wantPins[record.id] = record
	}
	if !reflect.DeepEqual(reply.Records, wantRecords) {
		t.Fatalf("records=%+v; want %+v", reply.Records, wantRecords)
	}
	pins := savedContexts(t, f, id)
	recordCount := 0
	revisionIDs := map[string]bool{}
	for _, revisionID := range reply.RevisionIDs {
		revisionIDs[revisionID] = true
	}
	for _, pin := range pins {
		if pin.Kind != "record" {
			continue
		}
		recordCount++
		record, ok := wantPins[pin.Revision]
		if !ok || pin.Layer != "harness" || pin.Path != record.fileName || pin.Hash != record.hash || pin.Content.Valid || !revisionIDs[pin.ID] {
			t.Fatalf("unexpected record pin: %+v; expected records=%+v", pin, want)
		}
		delete(wantPins, pin.Revision)
	}
	if recordCount != len(want) || len(wantPins) != 0 {
		t.Fatalf("record pins=%d; want %d; missing=%+v", recordCount, len(want), wantPins)
	}
	assertContextEvent(t, f, id, pins)
}

func TestContextAPIRecordSelection(t *testing.T) {
	for _, test := range []struct {
		name             string
		records, recent  int
		deleted, foreign bool
		omitRecords      bool
	}{
		{name: "newest_first_up_to_recent", records: 4, recent: 2},
		{name: "deleted_excluded_before_limit", records: 4, recent: 2, deleted: true},
		{name: "other_harness_excluded_before_limit", records: 4, recent: 2, foreign: true},
		{name: "zero_passes_no_records", records: 2, recent: 0},
		{name: "absent_configuration_passes_no_records", records: 2, omitRecords: true},
		{name: "fewer_records_than_recent", records: 2, recent: 10},
		{name: "maximum_recent", records: 52, recent: 50},
		{name: "no_records", recent: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, token, id, _ := reportFixture(t)
			input := contextInput(t, f, id)
			if test.omitRecords {
				execSQL(t, f, `UPDATE harness_profiles SET definition=json_remove(definition,'$.records')
					WHERE id=(SELECT profile_id FROM executions WHERE id=?)`, id)
			} else {
				setDefinition(t, f, id, "$.records", fmt.Sprintf(`{"keep":[],"pass":{"recent":%d}}`, test.recent))
			}
			sourceID, taskID := contextRecordSource(t, f)
			execSQL(t, f, `INSERT INTO harnesses(id,name,repo_url,repo_path,created_at,updated_at)
				VALUES ('other','Other','','','t','t')`)
			eligible := []contextRecordSeed{}
			for i := 1; i <= test.records; i++ {
				harnessID := "harness-a"
				if test.foreign && i == test.records {
					harnessID = "other"
				}
				record := seedContextRecord(t, f, sourceID, taskID, harnessID, i)
				if test.deleted && i == test.records {
					execSQL(t, f, `UPDATE harness_records SET deleted_at='2026-11-02T00:00:00Z' WHERE id=?`, record.id)
				} else if harnessID == "harness-a" {
					eligible = append(eligible, record)
				}
			}
			want := []contextRecordSeed{}
			for i := len(eligible) - 1; i >= 0 && len(want) < test.recent; i-- {
				want = append(want, eligible[i])
			}
			startStage(t, f, token, id, "work")
			status, body := requestContext(t, f, token, id, nil, input)
			assertContextRecords(t, f, id, status, body, want)
		})
	}
}

func TestContextAPIRecordsStayPinnedAfterQuestionAnswer(t *testing.T) {
	for _, recent := range []int{0, 2} {
		t.Run(fmt.Sprintf("recent_%d", recent), func(t *testing.T) {
			testContextAPIRecordsStayPinnedAfterQuestionAnswer(t, recent)
		})
	}
}

func testContextAPIRecordsStayPinnedAfterQuestionAnswer(t *testing.T, recent int) {
	f, token, id, _ := reportFixture(t)
	input := contextInput(t, f, id)
	setDefinition(t, f, id, "$.records", fmt.Sprintf(`{"keep":[],"pass":{"recent":%d}}`, recent))
	sourceID, taskID := contextRecordSource(t, f)
	older := seedContextRecord(t, f, sourceID, taskID, "harness-a", 1)
	newer := seedContextRecord(t, f, sourceID, taskID, "harness-a", 2)
	want := []contextRecordSeed{}
	if recent > 0 {
		want = []contextRecordSeed{newer, older}
	}
	attempt := startStage(t, f, token, id, "work")
	status, first := requestContext(t, f, token, id, nil, input)
	assertContextRecords(t, f, id, status, first, want)
	pins := savedContexts(t, f, id)

	report := finished("work", attempt, nil)
	report["work"].(map[string]any)["question"] = map[string]any{"question": "Continue?"}
	finishStage(t, f, token, id, report)
	decisionID := openDecisionID(t, f, id, "question")
	execSQL(t, f, `UPDATE harness_records SET deleted_at='2026-11-02T00:00:00Z' WHERE id=?`, newer.id)
	seedContextRecord(t, f, sourceID, taskID, "harness-a", 3)
	answer(t, f, decisionID, `{"action":"answer","text":"Continue","option":null}`, 200)
	status, body := requestClaim(t, f, token)
	resumed := requireClaim(t, status, body)
	if resumed.Execution.ID != id {
		t.Fatalf("resumed another Execution: %s; want %s", resumed.Execution.ID, id)
	}
	startStage(t, f, token, id, "work")
	status, second := requestContext(t, f, token, id, nil, input)
	assertContextRecords(t, f, id, status, second, want)
	if string(second) != string(first) || !reflect.DeepEqual(savedContexts(t, f, id), pins) {
		t.Fatalf("resume changed context: first=%s second=%s", first, second)
	}
}

// Spec 07 3.3 step 3 (Q210): when selected records share a file_name, the
// newest keeps it and each older one is renamed with its harness_records.id;
// the response fileName and the context_revisions source_path both carry the
// placed name. Records whose names do not collide keep their names.
func TestContextAPIRenamesCollidingRecordFileNames(t *testing.T) {
	f, token, id, _ := reportFixture(t)
	input := contextInput(t, f, id)
	setDefinition(t, f, id, "$.records", `{"keep":[],"pass":{"recent":10}}`)
	sourceID, taskID := contextRecordSource(t, f)
	seeds := []contextRecordSeed{}
	for i, name := range []string{"report.md", "notes", "report.md", "unique.md", "notes", "report.md"} {
		record := seedContextRecord(t, f, sourceID, taskID, "harness-a", i+1)
		execSQL(t, f, `UPDATE harness_records SET file_name=? WHERE id=?`, name, record.id)
		record.fileName = name
		seeds = append(seeds, record)
	}
	// Newest first: record-006 report.md, 005 notes, 004 unique.md, 003 report.md, 002 notes, 001 report.md.
	want := []contextRecordSeed{seeds[5], seeds[4], seeds[3], seeds[2], seeds[1], seeds[0]}
	want[3].fileName = "report-record-003.md"
	want[4].fileName = "notes-record-002"
	want[5].fileName = "report-record-001.md"
	startStage(t, f, token, id, "work")
	status, body := requestContext(t, f, token, id, nil, input)
	assertContextRecords(t, f, id, status, body, want)
}

func TestRecordPlacementNames(t *testing.T) {
	for _, test := range []struct {
		name  string
		files []string
		want  []string
	}{
		{name: "no_collision_unchanged", files: []string{"a.md", "b.md", "c"}, want: []string{"a.md", "b.md", "c"}},
		{name: "older_renamed_with_extension", files: []string{"a.md", "a.md"}, want: []string{"a.md", "a-r2.md"}},
		{name: "older_renamed_without_extension", files: []string{"a", "a"}, want: []string{"a", "a-r2"}},
		{name: "last_extension_only", files: []string{"a.tar.gz", "a.tar.gz"}, want: []string{"a.tar.gz", "a.tar-r2.gz"}},
		{name: "three_way", files: []string{"a.md", "b.md", "a.md", "a.md"}, want: []string{"a.md", "b.md", "a-r3.md", "a-r4.md"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			records := []store.HarnessRecord{}
			for i, file := range test.files {
				records = append(records, store.HarnessRecord{ID: fmt.Sprintf("r%d", i+1), FileName: file})
			}
			if got := recordPlacementNames(records); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("names=%v; want %v", got, test.want)
			}
		})
	}
}
