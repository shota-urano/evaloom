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
		ID                   string            `json:"id"`
		TaskID               string            `json:"taskId"`
		Seq                  int               `json:"seq"`
		Mode                 string            `json:"mode"`
		ResumePoint          *string           `json:"resumePoint"`
		ResumeHeadSHA        string            `json:"resumeHeadSha"`
		HarnessHeadSHA       string            `json:"harnessHeadSha"`
		VerifiedSHA          string            `json:"verifiedSha"`
		VerifyRun            *model.TestRun    `json:"verifyRun"`
		FreshNumber          *int              `json:"freshNumber"`
		ResumeBlock          *ClaimResumeBlock `json:"resumeBlock"`
		ResumeGateResults    []GateResult      `json:"resumeGateResults"`
		ResumeHarnessSummary *string           `json:"resumeHarnessSummary"`
	} `json:"execution"`
	Task struct {
		Title              string `json:"title"`
		Body               string `json:"body"`
		AcceptanceCriteria string `json:"acceptanceCriteria"`
		ShortName          string `json:"shortName"`
	} `json:"task"`
	Project model.Project `json:"project"`
	Profile struct {
		HarnessID       string          `json:"harnessId"`
		Adapter         string          `json:"adapter"`
		Version         string          `json:"version"`
		CommitSHA       string          `json:"commitSha"`
		Definition      json.RawMessage `json:"definition"`
		HarnessRepoPath string          `json:"harnessRepoPath"`
		ToolPermissions json.RawMessage `json:"toolPermissions"`
	} `json:"profile"`
	Previous *struct {
		WorktreePath *string `json:"worktreePath"`
		BranchName   *string `json:"branchName"`
		PRNumber     *int    `json:"prNumber"`
	} `json:"previous"`
	Worktree     *ClaimWorktree     `json:"worktree"`
	PullRequests []ClaimPullRequest `json:"pullRequests"`
	// Operation is set when the claim is an approved operation instead of
	// an Execution to run (spec 17 3.5.1). Only Execution.ID/TaskID and
	// Project accompany it.
	Operation         *ClaimOperation `json:"operation"`
	SetupAllowedHosts []string        `json:"-"`
	// Explicit null confirms that no earlier worktree was recorded. An omitted
	// field in an older/partial response cannot establish baseline absence.
	worktreeSnapshotPresent bool
}

func (c *Claim) UnmarshalJSON(data []byte) error {
	type claimJSON Claim
	var decoded claimJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields struct {
		Worktree json.RawMessage `json:"worktree"`
		Project  struct {
			SetupAllowedHosts []string `json:"setupAllowedHosts"`
		} `json:"project"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*c = Claim(decoded)
	c.worktreeSnapshotPresent = fields.Worktree != nil
	c.SetupAllowedHosts = fields.Project.SetupAllowedHosts
	return nil
}

type ClaimResumeBlock struct {
	ID        string          `json:"id"`
	Cause     string          `json:"cause"`
	HeadSHA   string          `json:"headSha"`
	Detail    json.RawMessage `json:"detail"`
	DecidedAt *string         `json:"decidedAt"`
}

type ClaimPullRequest struct {
	Number     int     `json:"number"`
	BranchName *string `json:"branchName"`
	Seq        int     `json:"seq"`
}

type ClaimWorktree struct {
	ID                  string             `json:"id"`
	Path                string             `json:"path"`
	BaseSHA             string             `json:"baseSha"`
	BranchName          string             `json:"branchName"`
	SetupStatus         string             `json:"setupStatus"`
	Baseline            *ClaimBaseline     `json:"baseline"`
	CopiedFiles         []model.CopiedFile `json:"copiedFiles"`
	Status              string             `json:"status"`
	ExpectedHeadSHA     string             `json:"expectedHeadSha"`
	PreparationComplete bool               `json:"preparationComplete"`
}

// ClaimBaseline carries the saved IDs supplied by the Control Plane (Q40).
// nil IDs on an ID-bearing run mean unverifiable, not count-only verification.
type ClaimBaseline struct {
	model.TestRun
	TestIDs []string `json:"testIds"`
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
		if claim.Operation != nil {
			// The approved operation is still being handled; the claim
			// keeps offering it until consume uses the approval.
			return nil
		}
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
	// Claims now carry the full saved test ID list. Artifact sizes are governed
	// by the Control Plane's configured upload limit, not a separate fixed
	// response cap that could reject an otherwise valid baseline.
	decoder := json.NewDecoder(response.Body)
	var reply Claim
	if err := decoder.Decode(&reply); err != nil || reply.Execution.ID == "" || reply.Execution.TaskID == "" {
		return nil, fmt.Errorf("runner: invalid claim response")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("runner: invalid claim response")
	}
	return &reply, nil
}
