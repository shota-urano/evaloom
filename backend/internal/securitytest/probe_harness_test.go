package securitytest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

// stageRun lays out one stage's request, out and scratch the way Runner
// does, and runs the committed bin/probe on the host with the contract env.
type stageRun struct {
	t                          *testing.T
	repo, workspace, inputs    string
	channel                    string
	replies                    chan authorizeAsk
	allowed                    bool
	approvalID, checkedContent *string
}

type authorizeAsk struct {
	Op         string  `json:"op"`
	Operation  string  `json:"operation"`
	ApprovalID *string `json:"approvalId"`
	ContentID  string  `json:"contentId"`
}

func newStageRun(t *testing.T, h ProbeHarness) *stageRun {
	t.Helper()
	root, err := os.MkdirTemp("/private/tmp", "e7j-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	r := &stageRun{t: t, repo: filepath.Join(root, "repo"), workspace: filepath.Join(root, "ws"), inputs: filepath.Join(root, "in"),
		channel: filepath.Join(root, "c.sock"), replies: make(chan authorizeAsk, 4)}
	for _, dir := range []string{r.repo, r.workspace, r.inputs} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteProbeHarness(r.repo, h); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", r.channel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var ask authorizeAsk
			_ = json.Unmarshal(line, &ask)
			r.replies <- ask
			reply := contract.AuthorizeResponse{Allowed: r.allowed}
			if !r.allowed {
				reply.Reason = "not_approved"
			}
			_ = json.NewEncoder(conn).Encode(reply)
			conn.Close()
		}
	}()
	return r
}

func (r *stageRun) run(stage string) (contract.Result, string) {
	r.t.Helper()
	dir := filepath.Join(filepath.Dir(r.repo), stage)
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0700); err != nil {
		r.t.Fatal(err)
	}
	request := map[string]any{"stage": stage, "task": map[string]any{"id": "task-1"},
		"paths":            map[string]any{"workspace": r.workspace, "out": out, "inputs": r.inputs},
		"checkedContentId": r.checkedContent, "approvals": []any{}}
	if r.approvalID != nil {
		request["approvals"] = []any{map[string]any{"operation": ProbeOperation, "id": *r.approvalID, "status": "approved"}}
	}
	data, _ := json.Marshal(request)
	requestPath := filepath.Join(dir, "request.json")
	if err := os.WriteFile(requestPath, data, 0600); err != nil {
		r.t.Fatal(err)
	}
	resultPath := filepath.Join(dir, "result.json")
	cmd := exec.Command(filepath.Join(r.repo, "bin", "probe"), stage)
	cmd.Dir = r.repo
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "EVALOOM_REQUEST=" + requestPath, "EVALOOM_RESULT=" + resultPath, "EVALOOM_CHANNEL=" + r.channel, "PROBE_SECRET_CANARY=fixture"}
	if output, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("probe %s: %v %s", stage, err, output)
	}
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		r.t.Fatal(err)
	}
	var result contract.Result
	if err := json.Unmarshal(raw, &result); err != nil {
		r.t.Fatalf("result %s: %v", raw, err)
	}
	if err := result.Validate(); err != nil {
		r.t.Fatalf("result %s: %v", raw, err)
	}
	return result, out
}

func TestProbeHarnessProfileIsValid(t *testing.T) {
	h := ProbeHarness{Secrets: map[string][]contract.Secret{"prepare": {{Slot: ProbeReadSlot, Access: "read"}},
		"publish": {{Slot: ProbeReadSlot, Access: "read"}, {Slot: ProbeWriteSlot, Access: "write"}}},
		Network: map[string][]string{"prepare": {"probe-destination.test:8080"}, "publish": {"probe-destination.test:8080"}}}
	repo := t.TempDir()
	if err := WriteProbeHarness(repo, h); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "evaloom", "profile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	definition, issues := contract.DecodeProfile(data)
	if len(issues) != 0 {
		t.Fatalf("probe Profile rejected: %+v", issues)
	}
	if definition.Stages.Publish.Network == nil || len(definition.Stages.Publish.Secrets) != 2 || definition.Stages.Check.Network != nil {
		t.Fatalf("declarations lost: %+v", definition.Stages)
	}
	// The Profile hook lets contract cases produce deliberately invalid input.
	h.Profile = func(p *contract.ProfileDefinition) { p.ContractVersion = "999" }
	if got := h.Definition().ContractVersion; got != "999" {
		t.Fatalf("Profile hook ignored: %s", got)
	}
	if err := WriteProbeHarness(t.TempDir(), ProbeHarness{Plans: map[string]json.RawMessage{"work": json.RawMessage(`[]`)}}); err == nil {
		t.Fatal("plan for a non-program stage accepted")
	}
}

func TestProbeHarnessProgramRunsStages(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// A loopback destination is enough for the host-side program test; the
	// Sandbox tests must pass Prerequisites, which reject it.
	destination := NewDestination(listener)
	defer destination.Close()
	checkPlan, _ := json.Marshal([]any{
		map[string]any{"id": "env", "kind": "env", "allowlist": []string{"PATH"}, "secrets": []string{"fixture"}},
		SecretProbe("secret-env", "fixture"),
		SecretProbe("secret-file", "prepared for task-1", "{{workspace}}/probe-prepared.txt"),
		SecretProbe("secret-absent", "never-present-value", "{{workspace}}/probe-prepared.txt", "{{workspace}}/missing"),
	})
	h := ProbeHarness{
		Plans: map[string]json.RawMessage{
			"prepare": json.RawMessage(`[{"id":"marker","kind":"exists","path":"{{workspace}}/probe-prepared.txt"},{"id":"absent","kind":"read","path":"{{workspace}}/missing"}]`),
			"check":   checkPlan,
		},
		Results:   map[string]map[string]any{"check": {"summary": "content {{contentId}}"}, "prepare": {"usage": "{{omit}}"}},
		Authorize: &ProbeAuthorize{Operation: ProbeOperation, Destination: destination.URL(DestinationOperationPath)},
	}
	r := newStageRun(t, h)
	prepared, out := r.run("prepare")
	if prepared.Status != model.StageSucceeded {
		t.Fatalf("prepare %+v", prepared)
	}
	observations := readObservations(t, filepath.Join(out, "probe-prepare.json"))
	if !observations["marker"] || observations["absent"] {
		t.Fatalf("prepare observations %v", observations)
	}
	checked, out := r.run("check")
	if checked.ContentID == nil || checked.Summary == nil || *checked.Summary != "content "+*checked.ContentID {
		t.Fatalf("check result %+v", checked)
	}
	envReport, err := os.ReadFile(filepath.Join(out, "probe-check.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envReport), `"secretPresent": true`) || strings.Contains(string(envReport), "=fixture") || strings.Contains(string(envReport), "never-present") {
		t.Fatalf("env observation %s", envReport)
	}
	if got := readObservations(t, filepath.Join(out, "probe-check.json")); !got["secret-env"] || !got["secret-file"] || got["secret-absent"] {
		t.Fatalf("secret observations %v", got)
	}
	r.checkedContent = checked.ContentID
	approval := "01JZ0000000000000000000000"
	r.approvalID = &approval

	r.allowed = false
	denied, out := r.run("publish")
	if denied.Status != model.StageFailed || denied.ErrorClass == nil || *denied.ErrorClass != "publish_failed" {
		t.Fatalf("denied publish %+v", denied)
	}
	if ask := <-r.replies; ask.Op != "authorize" || ask.Operation != ProbeOperation || ask.ApprovalID == nil || *ask.ApprovalID != approval || ask.ContentID != *checked.ContentID {
		t.Fatalf("authorize request %+v", ask)
	}
	if _, err := os.Stat(filepath.Join(out, "operation.json")); !errors.Is(err, os.ErrNotExist) || destination.Operations() != 0 {
		t.Fatalf("operation sent without allowed: %v %d", err, destination.Operations())
	}

	r.allowed = true
	published, _ := r.run("publish")
	<-r.replies
	if published.Status != model.StageSucceeded || destination.Operations() != 1 {
		t.Fatalf("allowed publish %+v operations=%d", published, destination.Operations())
	}
	requests := destination.Requests()
	if len(requests) != 1 || !strings.Contains(string(requests[0].Body), *checked.ContentID) {
		t.Fatalf("destination record %+v", requests)
	}
}

func readObservations(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Observations []struct {
			ID        string `json:"id"`
			Succeeded bool   `json:"succeeded"`
		} `json:"observations"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, o := range report.Observations {
		got[o.ID] = o.Succeeded
	}
	return got
}

func TestDestinationRecordsAndCounts(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := NewDestination(listener)
	defer d.Close()
	d.SetFile("/script.py", []byte("print('probe')"))
	var last string
	for _, c := range []struct{ method, path, body string }{{"GET", "/script.py", ""}, {"POST", DestinationOperationPath, `{"n":1}`}, {"GET", "/unknown", ""}, {"POST", DestinationOperationPath, `{"n":2}`}} {
		req, err := http.NewRequest(c.method, d.URL(c.path), strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if c.path == "/script.py" {
			last = string(data)
		}
	}
	if d.Operations() != 2 || len(d.Requests()) != 4 || d.Requests()[0].Path != "/script.py" {
		t.Fatalf("records %+v", d.Requests())
	}
	if !strings.HasPrefix(last, "print") {
		t.Fatalf("file not served: %q", last)
	}
}

type fatalRecorder struct{ messages []string }

func (f *fatalRecorder) Helper() {}
func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.messages = append(f.messages, fmt.Sprintf(format, args...))
}

type addrListener struct {
	net.Listener
	addr net.Addr
}

func (l addrListener) Addr() net.Addr { return l.addr }

type checkFunc func(context.Context) error

func (f checkFunc) Check(ctx context.Context) error { return f(ctx) }

// Spec 14 6: a missing prerequisite fails the test as unavailable; it is
// never a skip and never a pass.
func TestPrerequisitesFailInsteadOfSkipping(t *testing.T) {
	ok := checkFunc(func(context.Context) error { return nil })
	listen := func(ip string) *Destination {
		return &Destination{listener: addrListener{addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 8080}}}
	}
	for name, p := range map[string]Prerequisites{
		"no sandbox":           {},
		"sandbox unavailable":  {Sandbox: checkFunc(func(context.Context) error { return errors.New("docker daemon not running") })},
		"no destination":       {Sandbox: ok, RequireDestination: true},
		"nil destination":      {Sandbox: ok, RequireDestination: true, Destination: (*Destination)(nil)},
		"loopback destination": {Sandbox: ok, RequireDestination: true, Destination: listen("127.0.0.1")},
		"ipv6 loopback":        {Sandbox: ok, RequireDestination: true, Destination: listen("::1")},
		"unspecified address":  {Sandbox: ok, RequireDestination: true, Destination: listen("0.0.0.0")},
		"no model credentials": {Sandbox: ok, RequireModel: true},
	} {
		recorder := &fatalRecorder{}
		p.Require(context.Background(), recorder)
		if len(recorder.messages) != 1 || !strings.Contains(recorder.messages[0], "実行不能") {
			t.Fatalf("%s: %v", name, recorder.messages)
		}
	}
	recorder := &fatalRecorder{}
	Prerequisites{Sandbox: ok, RequireDestination: true, Destination: listen("192.0.2.10")}.Require(context.Background(), recorder)
	if len(recorder.messages) != 0 {
		t.Fatalf("available prerequisites failed: %v", recorder.messages)
	}
}

func TestStoreProbeSecretsForDeclaredStages(t *testing.T) {
	dir := t.TempDir()
	secrets := NewProbeSecrets()
	if err := secrets.Store(dir, ProbeHarnessID); err != nil {
		t.Fatal(err)
	}
	if secrets.Read == secrets.Write || !strings.HasPrefix(secrets.Read, "evaloom-canary-") {
		t.Fatalf("canaries %v", secrets.Values())
	}
	store := secretstore.New(dir)
	publish, err := store.ForStage(ProbeHarnessID, model.StagePublish, []contract.Secret{{Slot: ProbeReadSlot, Access: "read"}, {Slot: ProbeWriteSlot, Access: "write"}})
	if err != nil {
		t.Fatal(err)
	}
	env := publish.Environment()
	if env["EVALOOM_SECRET_PROBE_READ"] != secrets.Read || env["EVALOOM_SECRET_PROBE_WRITE"] != secrets.Write {
		t.Fatal("stored canaries are not resolved for publish")
	}
	// The store itself refuses probe_write for prepare (registered as write).
	if _, err := store.ForStage(ProbeHarnessID, model.StagePrepare, []contract.Secret{{Slot: ProbeWriteSlot, Access: "read"}}); err == nil {
		t.Fatal("probe_write resolved for prepare")
	}
}
