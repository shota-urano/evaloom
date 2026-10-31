package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

const maxConcurrentExecutions = 2

// Claim is the HTTP execution input. The Runner never opens the management DB.
type Claim struct {
	Execution struct {
		ID          string  `json:"id"`
		TaskID      string  `json:"taskId"`
		Seq         int     `json:"seq"`
		Mode        string  `json:"mode"`
		ResumePoint *string `json:"resumePoint"`
	} `json:"execution"`
	Task struct {
		Title              string `json:"title"`
		Body               string `json:"body"`
		AcceptanceCriteria string `json:"acceptanceCriteria"`
		ShortName          string `json:"shortName"`
	} `json:"task"`
	Project model.Project `json:"project"`
	Profile struct {
		HarnessID  string          `json:"harnessId"`
		Adapter    string          `json:"adapter"`
		Version    string          `json:"version"`
		CommitSHA  string          `json:"commitSha"`
		Definition json.RawMessage `json:"definition"`
	} `json:"profile"`
	Previous *struct {
		WorktreePath *string `json:"worktreePath"`
		BranchName   *string `json:"branchName"`
		PRNumber     *int    `json:"prNumber"`
	} `json:"previous"`
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

// A 204 creates no local execution. Failed/ambiguous claims are not replayed
// as executions; later polls ask the Control Plane for the next eligible work.
func (runner *Runner) claim(ctx context.Context) (*Claim, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, runner.claimURL, nil)
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
	const maxResponseBytes = 1 << 20
	content, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(content) > maxResponseBytes {
		return nil, fmt.Errorf("runner: invalid claim response")
	}
	var reply Claim
	if err := json.Unmarshal(content, &reply); err != nil || reply.Execution.ID == "" || reply.Execution.TaskID == "" {
		return nil, fmt.Errorf("runner: invalid claim response")
	}
	return &reply, nil
}
