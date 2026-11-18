package runner

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/dummy"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Spec 07 3.3 step 3 and 6: the work stage receives each pinned record
// through GET /api/executions/{id}/records/{artifactId}, places it read-only
// in records/, and skips (with a Runner log line) a record whose file is gone.
// The Control Plane, context API, record API and runWork are real; only the
// micro-harness is replaced by the deterministic dummy Adapter.
func TestWorkStagePlacesPinnedRecordsReadOnlyAndSkipsMissingFiles(t *testing.T) {
	p := newProfileFixture(t)
	p.def.Records = &contract.ProfileRecords{Keep: []string{}, Pass: contract.RecordsPass{Recent: 3}}
	p.definition()
	f := newLogControlPlane(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	claim := p.claim
	claim.Execution.ID = logExecution
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE harness_profiles SET definition=? WHERE id='fixture-profile'`, string(claim.Profile.Definition))
	exec(`UPDATE executions SET profile_id='fixture-profile',profile_commit_sha=?,harness_id='fixture-harness' WHERE id=?`, claim.Profile.CommitSHA, logExecution)
	// Records come from an earlier, finished Execution of the same Harness.
	source := ulid.Make().String()
	exec(`INSERT INTO tasks (id,project_id,title,body,acceptance_criteria,collaboration_mode,status,idempotency_key,created_at,updated_at)
		VALUES ('task-src','project','Earlier','','done','solo','completed','records-src','now','now')`)
	exec(`INSERT INTO executions (id,task_id,seq,mode,status,runner_id,harness_id,created_at)
		VALUES (?,'task-src',1,'fresh','succeeded','mac-mini','fixture-harness','now')`, source)
	type seed struct{ name, content, artifactID string }
	seeds := []seed{
		{name: "2026-11-01-ENG-1-report.md", content: "oldest report\n"},
		{name: "2026-11-02-ENG-2-report.md", content: "file removed later\n"},
		{name: "2026-11-03-ENG-3-report.md", content: "newest report\n"},
	}
	for i := range seeds {
		artifact, err := f.files.Save(t.Context(), store.Artifact{ID: ulid.Make().String(), ExecutionID: source,
			Stage: "check", Kind: "out_file", Name: "report.md"}, strings.NewReader(seeds[i].content))
		if err != nil {
			t.Fatal(err)
		}
		seeds[i].artifactID = artifact.ID
		exec(`INSERT INTO harness_records (id,harness_id,artifact_id,execution_id,task_id,file_name,created_at)
			VALUES (?,'fixture-harness',?,?,'task-src',?,?)`, ulid.Make().String(), artifact.ID, source, seeds[i].name,
			"2026-11-0"+string(rune('1'+i))+"T00:00:00Z")
		if i == 1 {
			if err := os.Remove(filepath.Join(f.dataDir, artifact.StoragePath)); err != nil {
				t.Fatal(err)
			}
		}
	}

	work := p.def.Stages.Work
	e := &executionExecutor{runner: r, claim: claim, config: f.config, paths: DefaultProposalPaths}
	e.config.MicroHarnessAuthFiles = map[string]string{work.MicroHarness: filepath.Join(t.TempDir(), "authentication")}
	in := StageInvocation{Stage: model.StageWork, Profile: LoadedProfile{Definition: p.def}, Directory: t.TempDir(), timeout: time.Minute}
	if err := os.WriteFile(filepath.Join(in.Directory, "request.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	records := filepath.Join(in.Directory, "records")
	started := false
	in.Adapter = dummy.NewWithStart(func(adapter.HarnessRequest, adapter.SandboxEnv) error {
		started = true
		entries, err := os.ReadDir(records)
		if err != nil {
			t.Errorf("records/: %v", err)
			return nil
		}
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		sort.Strings(names)
		if want := []string{seeds[0].name, seeds[2].name}; strings.Join(names, ",") != strings.Join(want, ",") {
			t.Errorf("records/ = %v; want %v (missing file not placed)", names, want)
		}
		for _, s := range []seed{seeds[0], seeds[2]} {
			name := filepath.Join(records, s.name)
			content, err := os.ReadFile(name)
			if err != nil || string(content) != s.content {
				t.Errorf("%s = %q, %v; want %q", s.name, content, err, s.content)
			}
			info, err := os.Lstat(name)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0222 != 0 {
				t.Errorf("%s mode = %v, %v; want a read-only regular file", s.name, info.Mode(), err)
			}
			if file, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0); err == nil {
				file.Close()
				t.Errorf("%s is writable", s.name)
			}
		}
		return nil
	})
	log, err := r.NewLog(logExecution, "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	outcome := e.runWork(t.Context(), in, sandbox.SandboxSpec{}, sandbox.ContainerConfig{}, log)
	if err := log.Finish(false); err != nil {
		t.Fatal(err)
	}
	// Records are reference material: a missing file does not stop the work.
	if outcome.RunnerError != nil || outcome.ExitCode == nil || *outcome.ExitCode != 0 || !started {
		t.Fatalf("work did not start and succeed: %+v, started=%v", outcome, started)
	}
	content, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), seeds[1].name) || !strings.Contains(string(content), seeds[1].artifactID) ||
		!strings.Contains(string(content), "missing") {
		t.Fatalf("Runner log does not note the missing record %s: %q", seeds[1].name, content)
	}
	for _, s := range []seed{seeds[0], seeds[2]} {
		if strings.Contains(string(content), s.name) {
			t.Errorf("Runner log reports placed record %s as missing: %q", s.name, content)
		}
	}
}

// The stage still fails when a pinned record has a file name that would
// escape records/; only a missing file (404) is skipped.
func TestWorkStageRejectsUnsafeRecordFileName(t *testing.T) {
	f := newLogControlPlane(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	e := &executionExecutor{runner: r, paths: DefaultProposalPaths}
	e.claim.Execution.ID = logExecution
	in := StageInvocation{Stage: model.StageWork, Directory: t.TempDir()}
	for _, name := range []string{"../escape.md", "sub/escape.md", "..", ""} {
		reply := workContext{}
		reply.Records = append(reply.Records, struct {
			FileName   string `json:"fileName"`
			ArtifactID string `json:"artifactId"`
		}{name, ulid.Make().String()})
		if err := e.placeWorkRecords(t.Context(), in, reply, nil); err == nil {
			t.Errorf("record file name %q was accepted", name)
		}
	}
	if _, err := os.Stat(filepath.Join(in.Directory, "escape.md")); !os.IsNotExist(err) {
		t.Fatalf("record escaped records/: %v", err)
	}
}

// Spec 07 3.3 step 3 (Q210): two pinned records sharing a file_name do not
// collide in records/. The newest keeps the name, the older one is placed as
// <name>-<harness_records.id>.<extension>, a non-colliding record keeps its
// name, and the work stage succeeds. The Control Plane and context API are real.
func TestWorkStagePlacesRecordsWithCollidingFileNames(t *testing.T) {
	p := newProfileFixture(t)
	p.def.Records = &contract.ProfileRecords{Keep: []string{}, Pass: contract.RecordsPass{Recent: 3}}
	p.definition()
	f := newLogControlPlane(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	claim := p.claim
	claim.Execution.ID = logExecution
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE harness_profiles SET definition=? WHERE id='fixture-profile'`, string(claim.Profile.Definition))
	exec(`UPDATE executions SET profile_id='fixture-profile',profile_commit_sha=?,harness_id='fixture-harness' WHERE id=?`, claim.Profile.CommitSHA, logExecution)
	source := ulid.Make().String()
	exec(`INSERT INTO tasks (id,project_id,title,body,acceptance_criteria,collaboration_mode,status,idempotency_key,created_at,updated_at)
		VALUES ('task-src','project','Earlier','','done','solo','completed','records-src','now','now')`)
	exec(`INSERT INTO executions (id,task_id,seq,mode,status,runner_id,harness_id,created_at)
		VALUES (?,'task-src',1,'fresh','succeeded','mac-mini','fixture-harness','now')`, source)
	type seed struct{ id, name, content string }
	seeds := []seed{
		{id: ulid.Make().String(), name: "report.md", content: "older report\n"},
		{id: ulid.Make().String(), name: "notes.md", content: "notes\n"},
		{id: ulid.Make().String(), name: "report.md", content: "newer report\n"},
	}
	for i, s := range seeds {
		artifact, err := f.files.Save(t.Context(), store.Artifact{ID: ulid.Make().String(), ExecutionID: source,
			Stage: "check", Kind: "out_file", Name: "report.md"}, strings.NewReader(s.content))
		if err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO harness_records (id,harness_id,artifact_id,execution_id,task_id,file_name,created_at)
			VALUES (?,'fixture-harness',?,?,'task-src',?,?)`, s.id, artifact.ID, source, s.name,
			"2026-11-0"+string(rune('1'+i))+"T00:00:00Z")
	}
	want := map[string]string{
		"report.md":                     seeds[2].content,
		"report-" + seeds[0].id + ".md": seeds[0].content,
		"notes.md":                      seeds[1].content,
	}

	work := p.def.Stages.Work
	e := &executionExecutor{runner: r, claim: claim, config: f.config, paths: DefaultProposalPaths}
	e.config.MicroHarnessAuthFiles = map[string]string{work.MicroHarness: filepath.Join(t.TempDir(), "authentication")}
	in := StageInvocation{Stage: model.StageWork, Profile: LoadedProfile{Definition: p.def}, Directory: t.TempDir(), timeout: time.Minute}
	if err := os.WriteFile(filepath.Join(in.Directory, "request.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	records := filepath.Join(in.Directory, "records")
	started := false
	in.Adapter = dummy.NewWithStart(func(adapter.HarnessRequest, adapter.SandboxEnv) error {
		started = true
		entries, err := os.ReadDir(records)
		if err != nil {
			t.Errorf("records/: %v", err)
			return nil
		}
		if len(entries) != len(want) {
			t.Errorf("records/ has %d files; want %d", len(entries), len(want))
		}
		for name, content := range want {
			got, err := os.ReadFile(filepath.Join(records, name))
			if err != nil || string(got) != content {
				t.Errorf("%s = %q, %v; want %q", name, got, err, content)
			}
		}
		return nil
	})
	log, err := r.NewLog(logExecution, "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	outcome := e.runWork(t.Context(), in, sandbox.SandboxSpec{}, sandbox.ContainerConfig{}, log)
	if err := log.Finish(false); err != nil {
		t.Fatal(err)
	}
	if outcome.RunnerError != nil || outcome.ExitCode == nil || *outcome.ExitCode != 0 || !started {
		t.Fatalf("work did not start and succeed: %+v, started=%v", outcome, started)
	}
	rows, err := f.db.Query(`SELECT source_path FROM context_revisions WHERE execution_id=? AND layer='harness' AND kind='record'`, logExecution)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	paths := map[string]bool{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			t.Fatal(err)
		}
		paths[path] = true
	}
	if len(paths) != len(want) {
		t.Fatalf("record source_paths=%v; want %v", paths, want)
	}
	for name := range want {
		if !paths[name] {
			t.Fatalf("record source_paths=%v; missing %s", paths, name)
		}
	}
}
