package runner

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

type instantProposalProcess struct{}

func (instantProposalProcess) Wait() error           { return nil }
func (instantProposalProcess) Stop() error           { return nil }
func (instantProposalProcess) Kill() error           { return nil }
func (instantProposalProcess) ExitCode() (int, bool) { return 0, true }

type recordingProposalHarness struct {
	destinations []string
	delay        time.Duration
}

func (h *recordingProposalHarness) ModelAPIDestinations() []string {
	return append([]string(nil), h.destinations...)
}

func (h *recordingProposalHarness) Run(ctx context.Context, in ProposalHarnessInput, launch ProposalLaunch) (ProposalHarnessOutcome, error) {
	spec, err := BuildProposalSandboxSpec(in)
	if err != nil {
		return ProposalHarnessOutcome{ErrorClass: strPtr("sandbox_error")}, nil
	}
	if len(spec.Secrets) != 0 {
		return ProposalHarnessOutcome{ErrorClass: strPtr("sandbox_error")}, nil
	}
	process, err := launch(ctx, spec, []string{"/bin/true"})
	if err != nil {
		return ProposalHarnessOutcome{ErrorClass: strPtr("sandbox_error")}, nil
	}
	if h.delay > 0 {
		timer := time.NewTimer(h.delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = process.Kill()
			_ = process.Wait()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ProposalHarnessOutcome{ErrorClass: strPtr("timeout")}, nil
			}
			return ProposalHarnessOutcome{}, ctx.Err()
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		_ = process.Kill()
		_ = process.Wait()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ProposalHarnessOutcome{ErrorClass: strPtr("timeout")}, nil
		}
		return ProposalHarnessOutcome{}, ctx.Err()
	}
	if err := os.MkdirAll(in.OutHost, 0700); err != nil {
		return ProposalHarnessOutcome{ErrorClass: strPtr("sandbox_error")}, nil
	}
	body, _ := json.Marshal(map[string]string{"label": "route:project", "reason": "fits"})
	if err := os.WriteFile(filepath.Join(in.OutHost, "proposal.json"), body, 0600); err != nil {
		return ProposalHarnessOutcome{ErrorClass: strPtr("sandbox_error")}, nil
	}
	_ = process.Wait()
	return ProposalHarnessOutcome{}, nil
}

func proposalClaim(id string, timeout int) ProposalClaim {
	return ProposalClaim{
		ID: id, TaskID: "task-routing", Trigger: "no_label",
		Task: struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}{Title: "title", Body: "body"},
		Candidates: json.RawMessage(`[{"label":"route:project","kind":"project","id":"project","description":"d"}]`),
		Proposer:   json.RawMessage(`{"microHarness":"codex","model":"test-model"}`),
		TimeoutSec: timeout,
	}
}

func immediateProposalLaunch(t *testing.T) ProposalLaunch {
	t.Helper()
	return func(_ context.Context, spec sandbox.SandboxSpec, _ []string) (sandbox.Process, error) {
		if len(spec.Secrets) != 0 {
			return nil, fmt.Errorf("secrets in proposal sandbox")
		}
		return instantProposalProcess{}, nil
	}
}

type runtimeInvocation struct {
	Args []string `json:"Args"`
	Env  []string `json:"Env"`
}

func backendModuleDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for dir := wd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			continue
		}
		if strings.Contains(string(content), "module github.com/shota-urano/evaloom/backend") {
			return dir
		}
	}
	t.Fatal("backend module directory not found")
	return ""
}

func compileSandboxRuntime(t *testing.T, binPath string) {
	t.Helper()
	cmd := exec.Command("go", "test", "-c", "-o", binPath, "./internal/sandbox")
	cmd.Dir = backendModuleDir(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile sandbox test runtime: %v\n%s", err, out)
	}
}

func sandboxRuntimeCLI(t *testing.T, fakeRoot string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sandbox-runtime")
	compileSandboxRuntime(t, bin)
	cli := filepath.Join(t.TempDir(), "container-runtime")
	script := fmt.Sprintf("#!/bin/sh\nexport SANDBOX_FAKE_ROOT=%q\nexec %q -test.run=^TestRuntimeHelper$ -- \"$@\"\n", fakeRoot, bin)
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return cli
}

func readSandboxRuntimeCalls(t *testing.T, fakeRoot string) []runtimeInvocation {
	t.Helper()
	file, err := os.Open(filepath.Join(fakeRoot, "calls.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var calls []runtimeInvocation
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var call runtimeInvocation
		if err := json.Unmarshal(scanner.Bytes(), &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return calls
}

func containerCreates(calls []runtimeInvocation) [][]string {
	var creates [][]string
	for _, call := range calls {
		if len(call.Args) > 2 && call.Args[0] == "container" && call.Args[1] == "create" {
			creates = append(creates, call.Args)
		}
	}
	return creates
}

func stubResultServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && len(r.URL.Path) > 0 {
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func TestProposalSandboxHasNoSecretsAndOnlyModelDestinations(t *testing.T) {
	server := stubResultServer(t)
	defer server.Close()
	modelHosts := []string{"api.openai.com:443", "auth.openai.com:443"}
	harness := &recordingProposalHarness{destinations: modelHosts}
	config := testConfig(t, server.URL)
	fakeRoot := t.TempDir()
	proxyBinary := filepath.Join(fakeRoot, "proxy")
	if err := os.WriteFile(proxyBinary, []byte("fixture proxy"), 0700); err != nil {
		t.Fatal(err)
	}
	services := ProposalServicesFromConfig(config)
	services.Resolve = func(string) (ProposalHarness, error) { return harness, nil }
	services.Container.RuntimePath = sandboxRuntimeCLI(t, fakeRoot)
	services.Container.ProxyBinary = proxyBinary
	runner, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	runner.client = server.Client()
	claim := proposalClaim(ulid.Make().String(), 300)
	if err := runner.ExecuteProposal(context.Background(), claim, services); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(runner.dataDir, ".runner-token")); err == nil {
		t.Fatal("runner token was duplicated into dataDir")
	}
	creates := containerCreates(readSandboxRuntimeCalls(t, fakeRoot))
	if len(creates) == 0 {
		t.Fatal("proposal sandbox never reached container create")
	}
	var workload []string
	for _, args := range creates {
		if slices.Contains(args, "egress") {
			for _, host := range modelHosts {
				if !slices.Contains(args, host) {
					t.Fatalf("egress create missing destination %q: %v", host, args)
				}
			}
			for _, host := range []string{"other.example:443", "profile-injected.example:443"} {
				if slices.Contains(args, host) {
					t.Fatalf("unexpected extra destination %q in egress", host)
				}
			}
			continue
		}
		if slices.Contains(args, "ingress") || slices.Contains(args, "/evaloom-proxy") {
			continue
		}
		workload = args
	}
	if workload == nil {
		t.Fatal("workload container create not found")
	}
	for i := 0; i+1 < len(workload); i++ {
		if workload[i] == "--env" && strings.HasPrefix(workload[i+1], "EVALOOM_SECRET_") {
			t.Fatalf("secret env in workload create: %s", workload[i+1])
		}
	}
	joined := strings.Join(workload, " ")
	for _, protected := range []string{config.TokenFile, filepath.Join(config.DataDir, "secrets")} {
		if strings.Contains(joined, protected) {
			t.Fatalf("protected path mounted into proposal sandbox: %s", protected)
		}
	}
}

func TestProposalInputContainsModel(t *testing.T) {
	claim := proposalClaim(ulid.Make().String(), 30)
	dir := t.TempDir()
	_, _, _, err := prepareProposalWorkspace(dir, claim, DefaultProposalPaths, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "proposals", claim.ID, "inputs", "proposal-input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Model != "test-model" {
		t.Fatalf("proposal input model=%q err=%v", payload.Model, err)
	}
	emptyClaim := proposalClaim(ulid.Make().String(), 30)
	emptyClaim.Proposer = json.RawMessage(`{"microHarness":"codex","model":""}`)
	server := stubResultServer(t)
	defer server.Close()
	runner, err := New(testConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	runner.client = server.Client()
	services := ProposalServicesFromConfig(testConfig(t, server.URL))
	services.Resolve = func(string) (ProposalHarness, error) {
		return &recordingProposalHarness{destinations: []string{"api.openai.com:443"}}, nil
	}
	if err := runner.ExecuteProposal(context.Background(), emptyClaim, services); err != nil {
		t.Fatal(err)
	}
}

func TestProposalStageMountRootsAreDedicated(t *testing.T) {
	t.Helper()
	claim := proposalClaim(ulid.Make().String(), 30)
	dir := t.TempDir()
	paths := DefaultProposalPaths
	_, _, roots, err := prepareProposalWorkspace(dir, claim, paths, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 6 {
		t.Fatalf("roots=%d want 6", len(roots))
	}
	want := []string{
		paths.Profile, paths.Workspace, paths.Out, paths.Inputs, paths.Records, paths.Scratch,
	}
	for _, target := range want {
		root, ok := roots[target]
		if !ok {
			t.Fatalf("missing root for %s", target)
		}
		if !strings.HasPrefix(root, filepath.Join(dir, "proposals", claim.ID)) {
			t.Fatalf("root %s outside proposal workspace", root)
		}
	}
	var hosts []string
	for _, root := range roots {
		hosts = append(hosts, root)
	}
	for i := 0; i < len(hosts); i++ {
		for j := i + 1; j < len(hosts); j++ {
			if hosts[i] == hosts[j] {
				t.Fatalf("overlapping stage roots: %s", hosts[i])
			}
		}
	}
}

func TestProposalResultReachesControlPlane(t *testing.T) {
	db, config, _ := controlPlaneFixture(t, nil)
	proposalID := ulid.Make().String()
	taskID := ulid.Make().String()
	insertRoutingProposal(t, db, proposalID, taskID, config.RunnerID)
	process, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	harness := &recordingProposalHarness{destinations: []string{"api.openai.com:443"}}
	if err := process.ExecuteProposal(context.Background(), proposalClaim(proposalID, 300), ProposalServices{
		Resolve: func(string) (ProposalHarness, error) { return harness, nil },
		Launch:  immediateProposalLaunch(t),
		Paths:   DefaultProposalPaths,
	}); err != nil {
		t.Fatal(err)
	}
	var status, label, reason string
	var errClass sql.NullString
	if err := db.QueryRow(`SELECT status, coalesce(proposed_label,''), coalesce(reason,''), error_class
		FROM route_proposals WHERE id=?`, proposalID).Scan(&status, &label, &reason, &errClass); err != nil {
		t.Fatal(err)
	}
	if status != "proposed" || label != "route:project" || reason != "fits" || errClass.Valid {
		t.Fatalf("proposal row=%s %s %s %v", status, label, reason, errClass)
	}
}

func TestProposalTimeoutReportsTimeout(t *testing.T) {
	db, config, _ := controlPlaneFixture(t, nil)
	proposalID := ulid.Make().String()
	taskID := ulid.Make().String()
	insertRoutingProposal(t, db, proposalID, taskID, config.RunnerID)
	process, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	harness := &recordingProposalHarness{destinations: []string{"api.openai.com:443"}, delay: 2 * time.Second}
	if err := process.ExecuteProposal(context.Background(), proposalClaim(proposalID, 1), ProposalServices{
		Resolve: func(string) (ProposalHarness, error) { return harness, nil },
		Launch:  immediateProposalLaunch(t),
		Paths:   DefaultProposalPaths,
	}); err != nil {
		t.Fatal(err)
	}
	var status string
	var errClass sql.NullString
	if err := db.QueryRow(`SELECT status, error_class FROM route_proposals WHERE id=?`, proposalID).
		Scan(&status, &errClass); err != nil || status != "failed" || !errClass.Valid || errClass.String != "timeout" {
		t.Fatalf("status=%s class=%v err=%v", status, errClass, err)
	}
}

func insertRoutingProposal(t *testing.T, db *sql.DB, proposalID, taskID, runnerID string) {
	t.Helper()
	now := "2026-11-12T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO tasks (id, source, title, body, acceptance_criteria, collaboration_mode, status,
		idempotency_key, created_at, updated_at) VALUES (?, 'linear', 't', 'b', '', 'solo', 'routing', ?, ?, ?)`,
		taskID, "linear:"+proposalID+":1", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO route_proposals (id, task_id, trigger, seen_labels, candidates, status, runner_id, created_at)
		VALUES (?, ?, 'no_label', '[]', '[{"label":"route:project"}]', 'running', ?, ?)`,
		proposalID, taskID, runnerID, now); err != nil {
		t.Fatal(err)
	}
}
