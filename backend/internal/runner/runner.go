package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	client                      *http.Client
	mu                          sync.Mutex
	executions                  map[string]context.CancelFunc
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
		return nil, fmt.Errorf("runner: read token file: %w", err)
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
		client: &http.Client{
			Timeout: heartbeatTimeout,
			// Redirects must not forward credentials or change POST semantics.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		executions: make(map[string]context.CancelFunc),
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

// Heartbeat sends the current state once and dispatches cancellation requests.
// On failure it leaves all executions running; the caller can resend with the
// same bootId. Run schedules subsequent heartbeats; report retries are separate.
func (runner *Runner) Heartbeat(ctx context.Context) error {
	runner.mu.Lock()
	ids := make([]string, 0, len(runner.executions))
	for id := range runner.executions {
		ids = append(ids, id)
	}
	runner.mu.Unlock()
	sort.Strings(ids)
	body, err := json.Marshal(struct {
		BootID              string   `json:"bootId"`
		RunningExecutionIDs []string `json:"runningExecutionIds"`
	}{runner.bootID, ids})
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
		CancelExecutionIDs []string `json:"cancelExecutionIds"`
	}
	const maxResponseBytes = 1 << 20
	content, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(content) > maxResponseBytes {
		return fmt.Errorf("runner: invalid heartbeat response")
	}
	if err := json.Unmarshal(content, &reply); err != nil || reply.CancelExecutionIDs == nil {
		return fmt.Errorf("runner: invalid heartbeat response")
	}
	for _, id := range reply.CancelExecutionIDs {
		runner.mu.Lock()
		cancel := runner.executions[id]
		runner.mu.Unlock()
		if cancel != nil {
			// Invoke outside the lock so cancellation may finish an execution.
			cancel()
		}
	}
	return nil
}
