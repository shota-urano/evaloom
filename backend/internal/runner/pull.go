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

const maxConcurrentExecutions = 2

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

// Run heartbeats immediately, then claims at most one job per poll when there
// is capacity. execute owns stage execution and result reporting; it must return
// only after the execution has stopped and must honor its context. It is never
// retried here. onError runs on the loop goroutine and may be nil.
func (runner *Runner) Run(ctx context.Context, execute func(context.Context, Claim), onError func(error)) error {
	if execute == nil {
		return fmt.Errorf("runner: execution handler is required")
	}
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
			err = runner.claimAndStart(ctx, execute, &workers)
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
	used := len(runner.executions)
	if runner.claimPending {
		used++
	}
	return used < maxConcurrentExecutions
}

func (runner *Runner) claimAndStart(ctx context.Context, execute func(context.Context, Claim), workers *sync.WaitGroup) error {
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
	executionCtx, cancel := context.WithCancel(ctx)
	runner.mu.Lock()
	if _, exists := runner.executions[claim.Execution.ID]; exists {
		runner.mu.Unlock()
		cancel()
		return fmt.Errorf("runner: claimed execution is already registered")
	}
	runner.executions[claim.Execution.ID] = cancel
	runner.claimPending = false
	runner.mu.Unlock()
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer runner.ForgetExecution(claim.Execution.ID)
		defer cancel()
		execute(executionCtx, *claim)
	}()
	return nil
}

// {"job":null} (or a 204) creates no local execution. Failed/ambiguous claims
// are not replayed as executions; later polls ask the Control Plane for the
// next eligible work. The request carries workspaceRoot (spec 04 3.3, Q134).
func (runner *Runner) claim(ctx context.Context) (*Claim, error) {
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
	var reply Claim
	if err := json.Unmarshal(job, &reply); err != nil {
		return nil, fmt.Errorf("runner: invalid claim response")
	}
	// Route proposals need the Proposal Sandbox (spec 05 3.13), which this
	// Runner does not have yet.
	if reply.Kind != "execution" {
		return nil, fmt.Errorf("runner: unsupported claim job kind %q", reply.Kind)
	}
	if reply.Execution.ID == "" || reply.Execution.TaskID == "" {
		return nil, fmt.Errorf("runner: invalid claim response")
	}
	return &reply, nil
}
