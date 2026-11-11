package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const maxConcurrentJobs = 2

// Claim is the execution job of the claim response (spec 04 3.3.1). The Runner
// never opens the management DB.
type Claim struct {
	Kind      string `json:"kind"`
	Execution struct {
		ID          string  `json:"id"`
		TaskID      string  `json:"taskId"`
		Seq         int     `json:"seq"`
		Mode        string  `json:"mode"`
		ResumeStage *string `json:"resumeStage"`
	} `json:"execution"`
	Task struct {
		ID                 string          `json:"id"`
		Title              string          `json:"title"`
		Body               string          `json:"body"`
		AcceptanceCriteria string          `json:"acceptanceCriteria"`
		Source             json.RawMessage `json:"source"`
	} `json:"task"`
	Route   json.RawMessage `json:"route"`
	Params  json.RawMessage `json:"params"`
	Profile struct {
		ID         string          `json:"id"`
		HarnessID  string          `json:"harnessId"`
		Version    string          `json:"version"`
		CommitSHA  string          `json:"commitSha"`
		TagName    string          `json:"tagName"`
		RepoPath   string          `json:"repoPath"`
		Definition json.RawMessage `json:"definition"`
	} `json:"profile"`
	Workspace struct {
		ID      string `json:"id"`
		Path    string `json:"path"`
		Created bool   `json:"created"`
	} `json:"workspace"`
	Limits struct {
		TimeoutSec map[string]int `json:"timeoutSec"`
	} `json:"limits"`
	Previous json.RawMessage `json:"previous"`
	Resume   json.RawMessage `json:"resume"`
}

// ProposalClaim is the proposal payload of spec 04 3.3.1. Candidates and
// proposer settings are passed through for the proposal executor to consume.
type ProposalClaim struct {
	ID      string `json:"id"`
	TaskID  string `json:"taskId"`
	Trigger string `json:"trigger"`
	Task    struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	} `json:"task"`
	Candidates json.RawMessage `json:"candidates"`
	Proposer   json.RawMessage `json:"proposer"`
	TimeoutSec int             `json:"timeoutSec"`
}

type claimedJob struct {
	Claim
	Proposal ProposalClaim `json:"proposal"`
}

// JobHandlers connects the pull loop to the stage framework and the Proposal
// Sandbox. Handlers own execution and result reporting, must honor their
// context, and must return only after the job has stopped. Neither is retried.
type JobHandlers struct {
	Execution     func(context.Context, Claim)
	RouteProposal func(context.Context, ProposalClaim)
}

// Run preserves the execution-only entry point until command wiring supplies
// both executors. It rejects proposal jobs without invoking execute.
func (runner *Runner) Run(ctx context.Context, execute func(context.Context, Claim), onError func(error)) error {
	if execute == nil {
		return fmt.Errorf("runner: execution handler is required")
	}
	return runner.runJobs(ctx, JobHandlers{Execution: execute}, onError)
}

// RunJobs heartbeats immediately, then claims at most one job per poll when
// there is capacity shared by executions and proposals (spec 05 3.2).
// onError runs on the loop goroutine and may be nil.
func (runner *Runner) RunJobs(ctx context.Context, handlers JobHandlers, onError func(error)) error {
	if handlers.Execution == nil || handlers.RouteProposal == nil {
		return fmt.Errorf("runner: execution and route proposal handlers are required")
	}
	return runner.runJobs(ctx, handlers, onError)
}

func (runner *Runner) runJobs(ctx context.Context, handlers JobHandlers, onError func(error)) error {
	runner.mu.Lock()
	if runner.loopRunning {
		runner.mu.Unlock()
		return fmt.Errorf("runner: pull loop is already running")
	}
	runner.loopRunning = true
	runner.mu.Unlock()
	var workers sync.WaitGroup
	defer func() {
		workers.Wait()
		runner.mu.Lock()
		runner.loopRunning = false
		runner.mu.Unlock()
	}()
	for ctx.Err() == nil {
		// Reconcile the boot identity before taking work, including after a
		// failed initial heartbeat. Existing work continues on network errors.
		err := runner.Heartbeat(ctx)
		if err == nil && ctx.Err() == nil {
			err = runner.claimAndStart(ctx, handlers, &workers)
		}
		if ctx.Err() != nil {
			break
		}
		if err != nil && onError != nil {
			onError(err)
		}
		timer := time.NewTimer(runner.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return nil
}

// runner.mu must be held. Count in-flight claims before the network call.
func (runner *Runner) hasCapacityLocked() bool {
	used := len(runner.executions) + len(runner.proposals)
	if runner.claimPending {
		used++
	}
	return used < maxConcurrentJobs
}

func (runner *Runner) claimAndStart(ctx context.Context, handlers JobHandlers, workers *sync.WaitGroup) error {
	runner.mu.Lock()
	if runner.claimPending || !runner.hasCapacityLocked() {
		runner.mu.Unlock()
		return nil
	}
	runner.claimPending = true
	runner.mu.Unlock()
	defer func() {
		runner.mu.Lock()
		runner.claimPending = false
		runner.mu.Unlock()
	}()
	claim, err := runner.claim(ctx)
	if err != nil || claim == nil {
		return err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	id := claim.Execution.ID
	registered := runner.executions
	execute := func() { handlers.Execution(jobCtx, claim.Claim) }
	if claim.Kind == "route_proposal" {
		if handlers.RouteProposal == nil {
			cancel()
			return fmt.Errorf("runner: route proposal handler is required")
		}
		id = claim.Proposal.ID
		registered = runner.proposals
		execute = func() { handlers.RouteProposal(jobCtx, claim.Proposal) }
	}
	runner.mu.Lock()
	if _, exists := registered[id]; exists {
		runner.mu.Unlock()
		cancel()
		return fmt.Errorf("runner: claimed %s is already registered", claim.Kind)
	}
	registered[id] = cancel
	runner.claimPending = false
	runner.mu.Unlock()
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer func() {
			runner.mu.Lock()
			delete(registered, id)
			runner.mu.Unlock()
		}()
		defer cancel()
		execute()
	}()
	return nil
}

// {"job":null} (or a 204) creates no local job. Failed/ambiguous claims
// are not replayed as jobs; later polls ask the Control Plane for the
// next eligible work. The request carries workspaceRoot (spec 04 3.3, Q134).
func (runner *Runner) claim(ctx context.Context) (*claimedJob, error) {
	body, err := json.Marshal(map[string]string{"workspaceRoot": runner.workspaceRoot})
	if err != nil {
		return nil, fmt.Errorf("runner: create claim request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, runner.claimURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("runner: create claim request")
	}
	request.Header.Set("Authorization", "Bearer "+runner.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := runner.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("runner: claim transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runner: claim returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(response.Body)
	var envelope map[string]json.RawMessage
	if err := decoder.Decode(&envelope); err != nil || len(envelope) != 1 {
		return nil, fmt.Errorf("runner: invalid claim response")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("runner: invalid claim response")
	}
	job, ok := envelope["job"]
	if !ok {
		return nil, fmt.Errorf("runner: invalid claim response")
	}
	if string(job) == "null" {
		return nil, nil
	}
	var reply claimedJob
	if err := json.Unmarshal(job, &reply); err != nil {
		return nil, fmt.Errorf("runner: invalid claim response")
	}
	switch reply.Kind {
	case "execution":
		if reply.Execution.ID == "" || reply.Execution.TaskID == "" {
			return nil, fmt.Errorf("runner: invalid claim response")
		}
	case "route_proposal":
		if reply.Proposal.ID == "" || reply.Proposal.TaskID == "" {
			return nil, fmt.Errorf("runner: invalid claim response")
		}
	default:
		return nil, fmt.Errorf("runner: unsupported claim job kind %q", reply.Kind)
	}
	return &reply, nil
}
