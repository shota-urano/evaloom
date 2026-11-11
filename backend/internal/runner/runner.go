package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const heartbeatTimeout = 5 * time.Second

// Runner keeps one boot identity for its lifetime. It neither listens for
// connections nor opens the Control Plane database.
type Runner struct {
	bootID, token, heartbeatURL string
	claimURL                    string
	controlPlaneURL, dataDir    string
	workspaceRoot               string
	client                      *http.Client
	mu                          sync.Mutex
	heartbeatMu                 sync.Mutex
	profileMu                   sync.Mutex
	executions                  map[string]context.CancelFunc
	proposals                   map[string]context.CancelFunc
	cleanedWorkspaces           map[string]struct{}
	claimPending                bool
	loopRunning                 bool
	pollInterval                time.Duration
}

func New(config Config) (*Runner, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(config.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("runner: cannot read token file")
	}
	token := strings.TrimSpace(string(content))
	secret, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != token {
		return nil, fmt.Errorf("runner: token file must contain one canonical 256-bit base64url token")
	}
	var boot [16]byte
	if _, err := rand.Read(boot[:]); err != nil {
		return nil, fmt.Errorf("runner: generate bootId: %w", err)
	}
	endpoint, err := url.JoinPath(config.ControlPlaneURL, "api", "runners", url.PathEscape(config.RunnerID), "heartbeat")
	if err != nil {
		return nil, fmt.Errorf("runner: invalid heartbeat URL")
	}
	claimURL, err := url.JoinPath(config.ControlPlaneURL, "api", "runners", url.PathEscape(config.RunnerID), "claim")
	if err != nil {
		return nil, fmt.Errorf("runner: invalid claim URL")
	}
	return &Runner{
		bootID: hex.EncodeToString(boot[:]), token: token, heartbeatURL: endpoint,
		claimURL: claimURL, pollInterval: 5 * time.Second,
		controlPlaneURL: config.ControlPlaneURL, dataDir: config.DataDir,
		workspaceRoot: config.WorkspaceRoot,
		client: &http.Client{
			Timeout: heartbeatTimeout,
			// Redirects must not forward credentials or change POST semantics.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		executions:        make(map[string]context.CancelFunc),
		proposals:         make(map[string]context.CancelFunc),
		cleanedWorkspaces: make(map[string]struct{}),
	}, nil
}

// RegisterExecution connects heartbeat cancellation to the current execution
// stage. Cancellation must be safe to repeat, as with context.CancelFunc.
// A pending claim reserves a slot too, so registration cannot race admission.
func (runner *Runner) RegisterExecution(id string, cancel context.CancelFunc) error {
	if id == "" || cancel == nil {
		return fmt.Errorf("runner: execution ID and cancellation function are required")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if _, exists := runner.executions[id]; exists {
		return fmt.Errorf("runner: execution is already registered")
	}
	if !runner.hasCapacityLocked() {
		return fmt.Errorf("runner: execution limit reached")
	}
	runner.executions[id] = cancel
	return nil
}

// ForgetExecution is called after the execution has stopped and reported its
// result. A cancellation request alone does not remove it from heartbeats.
func (runner *Runner) ForgetExecution(id string) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	delete(runner.executions, id)
}

// Heartbeat sends the current state once and dispatches cancellation and cleanup.
// Transport or response failures leave executions running and cleanup reports
// pending. Cleanup failures are returned after processing the other instructions.
// Run schedules subsequent heartbeats; other report retries are separate.
func (runner *Runner) Heartbeat(ctx context.Context) error {
	// Serialize exchanges so a concurrent heartbeat cannot consume cleanup
	// acknowledgements added after its request was sent.
	runner.heartbeatMu.Lock()
	defer runner.heartbeatMu.Unlock()
	runner.mu.Lock()
	ids := make([]string, 0, len(runner.executions))
	for id := range runner.executions {
		ids = append(ids, id)
	}
	proposalIDs := make([]string, 0, len(runner.proposals))
	for id := range runner.proposals {
		proposalIDs = append(proposalIDs, id)
	}
	cleaned := make([]string, 0, len(runner.cleanedWorkspaces))
	for id := range runner.cleanedWorkspaces {
		cleaned = append(cleaned, id)
	}
	runner.mu.Unlock()
	sort.Strings(ids)
	sort.Strings(proposalIDs)
	sort.Strings(cleaned)
	body, err := json.Marshal(struct {
		BootID                  string   `json:"bootId"`
		RunningExecutionIDs     []string `json:"runningExecutionIds"`
		RunningRouteProposalIDs []string `json:"runningRouteProposalIds"`
		// Older Control Planes reject this field, so omit an empty report.
		CleanedWorkspaceIDs []string `json:"cleanedWorkspaceIds,omitempty"`
	}{runner.bootID, ids, proposalIDs, cleaned})
	if err != nil {
		return fmt.Errorf("runner: encode heartbeat: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, runner.heartbeatURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("runner: create heartbeat request")
	}
	request.Header.Set("Authorization", "Bearer "+runner.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := runner.client.Do(request)
	if err != nil {
		// Do not log URLs, bodies or transport errors that may echo secrets.
		return fmt.Errorf("runner: heartbeat transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("runner: heartbeat returned HTTP %d", response.StatusCode)
	}
	var reply struct {
		CancelExecutionIDs     []string `json:"cancelExecutionIds"`
		CancelRouteProposalIDs []string `json:"cancelRouteProposalIds"`
		CleanWorkspaceIDs      []string `json:"cleanWorkspaceIds"`
	}
	const maxResponseBytes = 1 << 20
	content, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(content) > maxResponseBytes {
		return fmt.Errorf("runner: invalid heartbeat response")
	}
	if err := json.Unmarshal(content, &reply); err != nil || reply.CancelExecutionIDs == nil {
		return fmt.Errorf("runner: invalid heartbeat response")
	}
	runner.mu.Lock()
	for _, id := range cleaned {
		delete(runner.cleanedWorkspaces, id)
	}
	runner.mu.Unlock()
	for _, id := range reply.CancelExecutionIDs {
		runner.mu.Lock()
		cancel := runner.executions[id]
		runner.mu.Unlock()
		if cancel != nil {
			// Invoke outside the lock so cancellation may finish an execution.
			cancel()
		}
	}
	for _, id := range reply.CancelRouteProposalIDs {
		runner.mu.Lock()
		cancel := runner.proposals[id]
		runner.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	var cleanupErr error
	for _, id := range reply.CleanWorkspaceIDs {
		if err := runner.cleanWorkspace(id); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		runner.mu.Lock()
		runner.cleanedWorkspaces[id] = struct{}{}
		runner.mu.Unlock()
	}
	return cleanupErr
}
