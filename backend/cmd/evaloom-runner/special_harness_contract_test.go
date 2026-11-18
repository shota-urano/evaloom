package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
)

// specialHarnessFixture is the minimal special harness of spec 19 Q173. Its
// repository content lives in testdata/special-harness-fixture: the Profile
// declares prepare, work, check, publish and one approval, and the
// prepare/check/publish program is built from program/ and committed with the
// tag. Nothing in it calls an AI; the work stage runs through the
// deterministic test Adapter of newHarnessFixture.
func specialHarnessFixture(scenario string) fixtureHarness {
	const source = "testdata/special-harness-fixture"
	instructions, err := os.ReadFile(filepath.Join(source, "prompts", "work.md"))
	return fixtureHarness{instructions: string(instructions), taskBody: "special harness fixture scenario=" + scenario, populate: func(t *testing.T, repo, _ string) {
		if err != nil {
			t.Fatal(err)
		}
		err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			target := filepath.Join(repo, strings.TrimPrefix(path, source))
			if entry.IsDir() {
				return os.MkdirAll(target, 0700)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0600)
		})
		if err != nil {
			t.Fatal(err)
		}
		build := exec.Command("go", "build", "-o", filepath.Join(repo, "bin", "fixture"), "./"+source+"/program")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build fixture program: %v %s", err, output)
		}
	}}
}

func (f *executionFixture) scalar(query string, target any) {
	f.t.Helper()
	if err := f.db.QueryRow(query, f.id).Scan(target); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
}

func (f *executionFixture) stageFile(stage, name string) []byte {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.config.DataDir, "executions", f.id, stage, "out", name))
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}

func (f *executionFixture) authorization(stage string) contract.AuthorizeResponse {
	f.t.Helper()
	var reply contract.AuthorizeResponse
	if err := json.Unmarshal(f.stageFile(stage, "authorization.json"), &reply); err != nil {
		f.t.Fatal(err)
	}
	return reply
}

// preparedContentID is the identifier the fixture derives from the file its
// prepare stage wrote into the task workspace.
func (f *executionFixture) preparedFile() string {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.claims) == 0 {
		f.t.Fatal("no claim")
	}
	return filepath.Join(f.claims[0].Workspace.Path, "prepared.txt")
}

func (f *executionFixture) preparedContentID() string {
	f.t.Helper()
	data, err := os.ReadFile(f.preparedFile())
	if err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (f *executionFixture) noPublishedOutput(stage string) {
	f.t.Helper()
	if _, err := os.Stat(filepath.Join(f.config.DataDir, "executions", f.id, stage, "out", "published.md")); !os.IsNotExist(err) {
		f.t.Fatalf("publish operation ran: %v", err)
	}
}

// Spec 19 9 (Q173): the minimal special harness in testdata runs against the
// real Control Plane and Runner from prepare to publish, waits for approval,
// asks a question and resumes, and is refused right before publishing when
// the content changed after approval (content_changed).
func TestSpecialHarnessFixtureContract(t *testing.T) {
	t.Run("prepare_to_publish", func(t *testing.T) {
		f := newHarnessFixture(t, "success", specialHarnessFixture("success"))
		f.start()
		f.waitStatus("succeeded")
		f.stages([]string{"prepare-1", "work-1", "check-1", "publish-1"})
		var contentID, outputs, taskStatus string
		var approvals int
		f.scalar(`SELECT content_id FROM executions WHERE id=?`, &contentID)
		f.scalar(`SELECT outputs FROM executions WHERE id=?`, &outputs)
		f.scalar(`SELECT count(*) FROM approvals WHERE execution_id=?`, &approvals)
		f.scalar(`SELECT t.status FROM tasks t JOIN executions e ON e.task_id=t.id WHERE e.id=?`, &taskStatus)
		if contentID != f.preparedContentID() {
			t.Fatalf("content_id %s; want the checked content %s", contentID, f.preparedContentID())
		}
		if approvals != 0 || taskStatus != "completed" {
			t.Fatalf("approvals=%d task=%s", approvals, taskStatus)
		}
		if !strings.Contains(outputs, "https://example.invalid/fixture/") {
			t.Fatalf("publish outputs not recorded: %s", outputs)
		}
		// Even an operation without approval is asked right before it runs.
		if reply := f.authorization("publish-1"); !reply.Allowed {
			t.Fatalf("publish authorization %+v", reply)
		}
		if got := string(f.stageFile("publish-1", "published.md")); got != "checked "+contentID+"\n" {
			t.Fatalf("published %q", got)
		}
	})

	t.Run("approval", func(t *testing.T) {
		f := newHarnessFixture(t, "approval", specialHarnessFixture("approval"))
		f.start()
		f.waitStatus("waiting_approval")
		f.stages([]string{"prepare-1", "work-1", "check-1"})
		decision := f.decision("approval")
		var status, operation, approvalContent, contentID string
		f.scalar(`SELECT status FROM approvals WHERE execution_id=?`, &status)
		f.scalar(`SELECT operation FROM approvals WHERE execution_id=?`, &operation)
		f.scalar(`SELECT content_id FROM approvals WHERE execution_id=?`, &approvalContent)
		f.scalar(`SELECT content_id FROM executions WHERE id=?`, &contentID)
		if status != "pending" || operation != "publish_report" || approvalContent != f.preparedContentID() || contentID != approvalContent {
			t.Fatalf("approval %s %s content %s; execution content %s", status, operation, approvalContent, contentID)
		}
		f.owner("POST", "/api/decisions/"+decision+"/answer", `{"action":"approve","contentId":"`+approvalContent+`"}`, 200)
		f.waitStatus("succeeded")
		f.stages([]string{"prepare-1", "work-1", "check-1", "publish-1"})
		f.claimCount(2)
		var used int
		f.scalar(`SELECT status FROM approvals WHERE execution_id=?`, &status)
		f.scalar(`SELECT use_count FROM approvals WHERE execution_id=?`, &used)
		if status != "consumed" || used != 1 {
			t.Fatalf("approval %s use_count=%d", status, used)
		}
		if reply := f.authorization("publish-1"); !reply.Allowed {
			t.Fatalf("publish authorization %+v", reply)
		}
		f.stageFile("publish-1", "published.md")
	})

	t.Run("question", func(t *testing.T) {
		f := newHarnessFixture(t, "question", specialHarnessFixture("question"))
		f.start()
		f.waitStatus("blocked")
		f.stages([]string{"prepare-1", "work-1"})
		decision := f.decision("question")
		f.owner("POST", "/api/decisions/"+decision+"/answer", `{"action":"answer","text":"keep it","option":"keep"}`, 200)
		f.waitStatus("succeeded")
		f.stages([]string{"prepare-1", "work-1", "work-2", "check-1", "publish-1"})
		f.claimCount(2)
		data, err := os.ReadFile(filepath.Join(f.config.DataDir, "executions", f.id, "work-2", "request.json"))
		if err != nil {
			t.Fatal(err)
		}
		var request contract.Request
		if err := json.Unmarshal(data, &request); err != nil {
			t.Fatal(err)
		}
		if request.Answer == nil || request.Answer.DecisionID != decision || request.Answer.Text != "keep it" {
			t.Fatalf("resume answer %s", data)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.workRequests) != 2 || !strings.Contains(f.workRequests[1].Context.Instructions, `"text":"keep it"`) {
			t.Fatalf("answer not delivered to the work Adapter: %d requests", len(f.workRequests))
		}
		if f.claims[1].Execution.ID != f.id || f.claims[1].Execution.ResumeStage == nil || *f.claims[1].Execution.ResumeStage != "work" {
			t.Fatalf("resume claim %+v", f.claims[1].Execution)
		}
	})

	t.Run("content_changed", func(t *testing.T) {
		f := newHarnessFixture(t, "content_changed", specialHarnessFixture("content_changed"))
		f.start()
		f.waitStatus("waiting_approval")
		decision := f.decision("approval")
		var approved string
		f.scalar(`SELECT content_id FROM approvals WHERE execution_id=?`, &approved)
		if approved != f.preparedContentID() {
			t.Fatalf("approval content %s; want %s", approved, f.preparedContentID())
		}
		// The content changes after the check; the owner approves what was checked.
		if err := os.WriteFile(f.preparedFile(), []byte("changed after approval\n"), 0600); err != nil {
			t.Fatal(err)
		}
		f.owner("POST", "/api/decisions/"+decision+"/answer", `{"action":"approve","contentId":"`+approved+`"}`, 200)
		f.waitStatus("failed")
		f.stages([]string{"prepare-1", "work-1", "check-1", "publish-1"})
		var errorClass, status, reason, events string
		var used int
		f.scalar(`SELECT error_class FROM executions WHERE id=?`, &errorClass)
		f.scalar(`SELECT status FROM approvals WHERE execution_id=?`, &status)
		f.scalar(`SELECT coalesce(invalidated_reason,'') FROM approvals WHERE execution_id=?`, &reason)
		f.scalar(`SELECT use_count FROM approvals WHERE execution_id=?`, &used)
		f.scalar(`SELECT coalesce(group_concat(payload),'') FROM execution_events WHERE execution_id=? AND type='approval_consumed'`, &events)
		if errorClass != "publish_failed" {
			t.Fatalf("error_class %s; want publish_failed", errorClass)
		}
		if status != "invalidated" || reason != "content_changed" || used != 0 {
			t.Fatalf("approval %s (%s) use_count=%d; want invalidated (content_changed)", status, reason, used)
		}
		if !strings.Contains(events, "content_changed") {
			t.Fatalf("approval_consumed events %s", events)
		}
		if reply := f.authorization("publish-1"); reply.Allowed || reply.Reason != "content_changed" {
			t.Fatalf("publish authorization %+v; want content_changed", reply)
		}
		f.noPublishedOutput("publish-1")
	})
}
