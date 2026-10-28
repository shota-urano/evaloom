// Package dummy supplies a deterministic, in-memory harness for extension tests.
// It is imported only by tests and is never registered by production binaries.
// It does not exercise CLI execution or sandbox isolation (spec 14's probes).
package dummy

import (
	"context"
	"fmt"
	"sync"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
)

type harness struct {
	mu       sync.Mutex
	sessions map[adapter.SessionID]session
}

type session struct {
	result adapter.HarnessResult
	log    adapter.LogLine
}

// New returns only the shared contract, so consumers need no concrete type.
func New() adapter.Adapter { return &harness{sessions: make(map[adapter.SessionID]session)} }

func (h *harness) Start(ctx context.Context, req adapter.HarnessRequest, env adapter.SandboxEnv) (adapter.SessionID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := req.Validate(); err != nil {
		return "", err
	}
	if env.Worktree == "" || env.Worktree != req.Project.Workdir {
		return "", fmt.Errorf("dummy: worktree must match request")
	}
	id := adapter.SessionID(ulid.Make().String())
	s := session{
		result: adapter.HarnessResult{
			ContractVersion: adapter.ContractVersion, Status: adapter.ResultSucceeded,
			SessionID: id, Summary: "dummy completed " + req.ExecutionID,
			Commits: []string{}, LogArtifactIDs: []string{}, ArtifactIDs: []string{},
		},
		log: adapter.LogLine{Stream: "stdout", Text: "dummy ran in " + env.Worktree},
	}
	h.mu.Lock()
	h.sessions[id] = s
	h.mu.Unlock()
	return id, nil
}

func (h *harness) get(id adapter.SessionID) (session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok {
		return session{}, fmt.Errorf("dummy: unknown session")
	}
	return s, nil
}

func (h *harness) Status(id adapter.SessionID) (adapter.SessionStatus, error) {
	if _, err := h.get(id); err != nil {
		return "", err
	}
	return adapter.SessionSucceeded, nil
}

func (h *harness) Logs(id adapter.SessionID) (<-chan adapter.LogLine, error) {
	s, err := h.get(id)
	if err != nil {
		return nil, err
	}
	lines := make(chan adapter.LogLine, 1)
	lines <- s.log
	close(lines)
	return lines, nil
}

func (h *harness) Cancel(id adapter.SessionID) error {
	// Start completes synchronously; a known session has already stopped.
	_, err := h.get(id)
	return err
}

func (h *harness) Result(id adapter.SessionID) (adapter.HarnessResult, error) {
	s, err := h.get(id)
	return s.result, err
}

func (*harness) Capabilities() adapter.Capabilities { return adapter.Capabilities{} }
