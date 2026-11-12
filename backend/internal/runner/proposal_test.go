package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	spec         *sandbox.SandboxSpec
}

func (h *recordingProposalHarness) ModelAPIDestinations() []string {
	return append([]string(nil), h.destinations...)
}

func (h *recordingProposalHarness) Run(ctx context.Context, in ProposalHarnessInput, launch ProposalLaunch) (ProposalHarnessOutcome, error) {
	spec, err := BuildProposalSandboxSpec(in)
	if err != nil {
		return ProposalHarnessOutcome{ErrorClass: strPtr("sandbox_error")}, nil
	}
	if h.spec != nil {
		*h.spec = spec
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

func immediateProposalLaunch(t *testing.T, modelHosts []string, capture *sandbox.SandboxSpec) ProposalLaunch {
	t.Helper()
	return func(_ context.Context, spec sandbox.SandboxSpec, _ []string) (sandbox.Process, error) {
		spec.AllowedDestinations = append([]string(nil), modelHosts...)
		if capture != nil {
			*capture = spec
		}
		return instantProposalProcess{}, nil
	}
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
	var spec sandbox.SandboxSpec
	modelHosts := []string{"api.openai.com:443", "auth.openai.com:443"}
	harness := &recordingProposalHarness{destinations: modelHosts, spec: &spec}
	runner, err := New(testConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	runner.client = server.Client()
	claim := proposalClaim(ulid.Make().String(), 300)
	if err := runner.ExecuteProposal(context.Background(), claim, ProposalServices{
		Resolve: func(string) (ProposalHarness, error) { return harness, nil },
		Launch:  immediateProposalLaunch(t, modelHosts, &spec),
	}); err != nil {
		t.Fatal(err)
	}
	if len(spec.Secrets) != 0 {
		t.Fatalf("proposal sandbox received secrets: %+v", spec.Secrets)
	}
	if spec.Kind != sandbox.Proposal || spec.Network != sandbox.NetworkHarness {
		t.Fatalf("unexpected sandbox kind/network: %s %s", spec.Kind, spec.Network)
	}
	if !slices.Equal(spec.AllowedDestinations, modelHosts) {
		t.Fatalf("launch destinations=%v want %v", spec.AllowedDestinations, modelHosts)
	}
	for key := range spec.Env {
		if key == "EVALOOM_SECRET_FOO" {
			t.Fatal("secret env leaked into proposal sandbox")
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
		Launch:  immediateProposalLaunch(t, harness.destinations, nil),
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
		Launch:  immediateProposalLaunch(t, harness.destinations, nil),
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
