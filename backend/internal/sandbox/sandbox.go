// Package sandbox defines the Runner's isolated process boundary (spec 09).
// It has no host-execution fallback and does not persist execution state.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

type SandboxKind string

const (
	// Harness is retained for the existing Adapter boundary.
	Harness  SandboxKind = "harness"
	Prepare  SandboxKind = "prepare"
	Work     SandboxKind = "work"
	Check    SandboxKind = "check"
	Publish  SandboxKind = "publish"
	Proposal SandboxKind = "proposal"
)

// NetworkPolicy's zero value denies all networking, for every Kind.
type NetworkPolicy string

const (
	NetworkNone NetworkPolicy = "none"
	// NetworkHarness uses the selected Adapter declaration for Work/Proposal.
	// Legacy Harness receives the same declaration from its Adapter launcher.
	NetworkHarness NetworkPolicy = "harness"
	// NetworkDeclared uses pinned Profile destinations for Prepare/Publish.
	NetworkDeclared NetworkPolicy = "declared"
)

// ResourceLimits are supplied by the trusted caller. There are no Q15 defaults.
// CPU is a quota in CPUs, MemoryBytes includes swap, and Time is wall time,
// measured from Run (including image inspection and container creation).
type ResourceLimits struct {
	CPU         float64
	MemoryBytes int64
	Time        time.Duration
}

// Mount describes a stage's visibility, using the trusted Runner's request paths.
type Mount struct {
	HostPath  string
	GuestPath string
	Writable  bool
}

type SandboxSpec struct {
	Mounts      []Mount
	Secrets     map[string]string
	ChannelPath string
	Kind        SandboxKind
	Worktree    string
	GitDir      string
	ConfigDir   string
	// True only for an execution's disposable harness configuration copy.
	ConfigWritable bool
	Env            map[string]string
	Network        NetworkPolicy
	// Exact DNS host:port destinations from the pinned Profile (Prepare/Publish)
	// or legacy Harness launcher. Work/Proposal replace this with ModelAPI's
	// declaration. This is not a Project Registry or public HTTP API field.
	AllowedDestinations []string
	Limits              ResourceLimits
}

type Sandbox interface {
	Run(context.Context, SandboxSpec, []string) (Process, error)
}

// Process is structurally compatible with runner.SandboxProcess. Wait may be
// called repeatedly and returns only after the entire container has stopped.
// Stop requests TERM; Kill forcibly removes the entire container. Both are
// bounded by the caller's control-command timeout and are safe after Wait.
// Output is streamed to the ContainerConfig writers. ExitCode is valid after
// Wait only if the runtime supplied a confirmed workload exit status.
type Process interface {
	Wait() error
	Stop() error
	Kill() error
	ExitCode() (int, bool)
}

// Error carries only a safe diagnostic, never raw runtime output (which may
// contain secrets). Status/ErrorClass use the existing spec 02 classifications.
// A deadline remains discoverable with errors.Is for Runner's supervisor.
type Error struct {
	Op    string
	Cause error
}

func (e *Error) Error() string { return "sandbox: " + e.Op }
func (e *Error) Unwrap() error { return e.Cause }
func (e *Error) Status() model.ExecutionStatus {
	if errors.Is(e.Cause, context.DeadlineExceeded) {
		return model.ExecutionTimedOut
	}
	if errors.Is(e.Cause, context.Canceled) {
		return model.ExecutionCancelled
	}
	return model.ExecutionFailed
}
func (e *Error) ErrorClass() model.ErrorClass {
	switch e.Status() {
	case model.ExecutionTimedOut:
		return model.ErrorTimeout
	case model.ExecutionCancelled:
		return model.ErrorCancelled
	default:
		return model.ErrorSandbox
	}
}

// ExitError is an ordinary workload failure, to be classified by its caller
// (harness/execution_failed).
type ExitError struct{ Code int }

func (e *ExitError) Error() string {
	return fmt.Sprintf("sandbox: command exited with code %d", e.Code)
}
