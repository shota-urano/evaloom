// Package adapter defines the harness contract and resolves implementations by name.
// Concrete harnesses belong in subpackages such as adapter/codex.
package adapter

import "context"

// Adapter provides the common operations used by Runner. Implementations report
// CLI failures through HarnessResult.Status and ErrorClass, rather than panicking.
type Adapter interface {
	Start(ctx context.Context, req HarnessRequest, env SandboxEnv) (SessionID, error)
	Status(id SessionID) (SessionStatus, error)
	Logs(id SessionID) (<-chan LogLine, error)
	Cancel(id SessionID) error
	Result(id SessionID) (HarnessResult, error)
	Capabilities() Capabilities
}

type Capabilities struct {
	Streaming              bool     // Logs can be streamed incrementally.
	Resume                 bool     // Interrupted sessions can be resumed.
	TokenUsage             bool     // Token counts can be reported.
	NativeInstructionFiles []string // Instruction files read by the CLI itself.
}

// SessionID is issued by the adapter and stored as executions.harness_session_id.
type SessionID string

// SessionStatus describes the harness session, not the Runner execution stages.
// Cancellation and timeout are reported as failed results with an ErrorClass.
type SessionStatus string

const (
	SessionRunning   SessionStatus = "running"
	SessionSucceeded SessionStatus = "succeeded"
	SessionFailed    SessionStatus = "failed"
)

// LogLine contains raw CLI output. Runner must remove secrets before persistence.
type LogLine struct {
	Stream string // stdout or stderr
	Text   string
}

// SandboxEnv contains paths as seen inside the prepared harness sandbox and its
// allowlisted environment. It does not create a sandbox or execute host commands.
type SandboxEnv struct {
	Worktree  string
	ConfigDir string
	Env       map[string]string
}
