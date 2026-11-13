package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
)

func TestDeclaredWorkDirectoriesPinEachInstalledFile(t *testing.T) {
	p := newProfileFixture(t)
	p.write("work/prompts/first.md", "first\r\n", 0600)
	p.write("work/prompts/last.md", "last\n", 0600)
	p.def.Stages.Work.Instructions = []string{"work/prompts/", "work/instructions.md"}
	p.def.Stages.Work.Skills = []string{"work/skills/", "work/skills/test/SKILL.md", "./work/skills"}
	p.def.Stages.Work.Settings = []string{"./work/settings.json"}
	p.commit()
	p.definition()
	pinnedSHA := p.claim.Profile.CommitSHA
	// Changes to tags, HEAD, index and working tree do not change this reader's
	// pin. Profile preflight separately rejects a moved tag before stage start.
	p.write("work/prompts/first.md", "later committed content", 0600)
	p.write("work/skills/test/later.md", "not in pinned commit", 0600)
	p.commit()
	p.git("tag", "-f", "profile/v1.0.0")
	p.write("work/instructions.md", "uncommitted content", 0600)
	p.claim.Profile.CommitSHA = pinnedSHA

	f := newLogControlPlane(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	claim := p.claim
	claim.Execution.ID = logExecution
	if _, err := f.db.Exec(`UPDATE harness_profiles SET definition=? WHERE id='fixture-profile'`, string(claim.Profile.Definition)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET profile_id='fixture-profile',profile_commit_sha=?,harness_id='fixture-harness' WHERE id=?`, pinnedSHA, logExecution); err != nil {
		t.Fatal(err)
	}
	work := p.def.Stages.Work
	paths := append(append(append([]string{}, work.Instructions...), work.Skills...), work.Settings...)
	files, err := harnesscontext.Read(t.Context(), p.repo, pinnedSHA, paths)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := harnessenv.Create(t.Context(), harnessenv.Config{
		Root: t.TempDir(), CommitSHA: pinnedSHA, ConfigVariable: "CODEX_HOME", Files: files,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer environment.Close()
	installed, err := environment.Files(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	e := &executionExecutor{runner: r, claim: claim}
	in := StageInvocation{Profile: LoadedProfile{Definition: p.def}}
	first, err := e.pinWorkContext(t.Context(), in, installed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.pinWorkContext(t.Context(), in, installed)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated pin = (%+v, %v), want %+v", second, err, first)
	}
	want := map[string]string{
		"instructions:work/prompts/first.md": "first\r\n",
		"instructions:work/prompts/last.md":  "last\n",
		"instructions:work/instructions.md":  "committed instructions\n",
		"skill:work/skills/test/SKILL.md":    "committed skill\n",
		"settings:work/settings.json":        "{}",
	}
	rows, err := f.db.Query(`SELECT kind,source_path,source_revision,content_hash,content IS NULL FROM context_revisions WHERE execution_id=? AND layer='harness'`, logExecution)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var kind, name, revision, hash string
		var noContent bool
		if err := rows.Scan(&kind, &name, &revision, &hash, &noContent); err != nil {
			t.Fatal(err)
		}
		content, ok := want[kind+":"+name]
		digest := sha256.Sum256([]byte(content))
		if !ok || revision != pinnedSHA || hash != hex.EncodeToString(digest[:]) || !noContent {
			t.Errorf("unexpected pin: %s:%s revision=%s hash=%s noContent=%v", kind, name, revision, hash, noContent)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != len(want) || len(installed) != len(want) {
		t.Fatalf("pins=%d installed=%d, want exactly %d declared files", count, len(installed), len(want))
	}
}

func TestWorkContextRejectsUndeclaredAndIncompleteFileReports(t *testing.T) {
	f := newLogControlPlane(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	p := newProfileFixture(t)
	if _, err := f.db.Exec(`UPDATE harness_profiles SET definition=? WHERE id='fixture-profile'`, string(p.claim.Profile.Definition)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET profile_id='fixture-profile',profile_commit_sha=? WHERE id=?`, p.claim.Profile.CommitSHA, logExecution); err != nil {
		t.Fatal(err)
	}
	type file struct {
		Kind   string `json:"kind"`
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	hash := strings.Repeat("a", 64)
	valid := []file{{"instructions", "work/instructions.md", hash}, {"skill", "work/skills/test/SKILL.md", hash}, {"settings", "work/settings.json", hash}}
	for _, test := range []struct {
		name  string
		files []file
	}{
		{"check program", append(append([]file{}, valid...), file{"skill", "bin/check", hash})},
		{"prefix sibling", append(append([]file{}, valid...), file{"skill", "work/skills-extra/private", hash})},
		{"missing directory files", []file{valid[0], valid[2]}},
		{"wrong kind", []file{valid[0], {"instructions", valid[1].Path, hash}, valid[2]}},
		{"duplicate", append(append([]file{}, valid...), valid[1])},
		{"noncanonical path", []file{valid[0], {"skill", "work/skills/../settings.json", hash}, valid[2]}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Use the real authenticated API, including its declaration checks.
			body, err := json.Marshal(map[string]any{"profileCommitSha": p.claim.Profile.CommitSHA, "files": test.files})
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequestWithContext(t.Context(), "POST", f.server.URL+"/api/executions/"+logExecution+"/context", strings.NewReader(string(body)))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+r.token)
			request.Header.Set("Content-Type", "application/json")
			response, err := r.client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ = io.ReadAll(response.Body)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("context status=%d body=%s, want 400", response.StatusCode, body)
			}
			var count int
			if err := f.db.QueryRow(`SELECT count(*) FROM context_revisions WHERE execution_id=?`, logExecution).Scan(&count); err != nil || count != 0 {
				t.Fatalf("rejected report left %d context rows, err=%v", count, err)
			}
		})
	}
}

func TestMissingDeclaredWorkFileFailsExecutionInControlPlane(t *testing.T) {
	f := newLogControlPlane(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	_, claim, _, _ := stageFixture(t, false)
	var def contract.ProfileDefinition
	if err := json.Unmarshal(claim.Profile.Definition, &def); err != nil {
		t.Fatal(err)
	}
	def.Stages.Work.Instructions = append(def.Stages.Work.Instructions, "work/missing.md")
	claim.Profile.Definition, err = json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	claim.Execution.ID, claim.Execution.TaskID, claim.Task.ID = logExecution, "task-001", "task-001"
	if _, err := f.db.Exec(`UPDATE harness_profiles SET definition=? WHERE id='fixture-profile'`, string(claim.Profile.Definition)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET status='claimed',profile_id='fixture-profile',profile_commit_sha=? WHERE id=?`, claim.Profile.CommitSHA, logExecution); err != nil {
		t.Fatal(err)
	}
	// Exercise ExecuteClaim, Profile preflight, stage reporting and the actual
	// Control Plane transitions. No stage executor or reporting is replaced.
	err = r.ExecuteClaim(t.Context(), f.config, claim, func(c WorkAdapterConfig) *adapter.Registry {
		adapters := &adapter.Registry{}
		if err := adapters.Register("codex", codex.New(codex.Config{
			Launch: c.Launch, ResolveConfig: c.ResolveConfig, ResolveAuthentication: c.ResolveAuthentication,
			ResolveToolPermissions: c.ResolveToolPermissions, CancelGrace: c.CancelGrace,
		})); err != nil {
			t.Fatal(err)
		}
		return adapters
	})
	if err != nil {
		t.Fatal(err)
	}
	var status, taskStatus, errorClass string
	if err := f.db.QueryRow(`SELECT e.status,t.status,e.error_class FROM executions e JOIN tasks t ON t.id=e.task_id WHERE e.id=?`, logExecution).Scan(&status, &taskStatus, &errorClass); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || taskStatus != "failed" || errorClass != "config_error" {
		t.Fatalf("execution=%s task=%s error=%s, want failed/failed/config_error", status, taskStatus, errorClass)
	}
	var pins int
	if err := f.db.QueryRow(`SELECT count(*) FROM context_revisions WHERE execution_id=?`, logExecution).Scan(&pins); err != nil || pins != 0 {
		t.Fatalf("failed configuration left %d context rows, err=%v", pins, err)
	}
}
