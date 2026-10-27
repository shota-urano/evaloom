// Package model defines the shared enum values from spec 02.
package model

import (
	"encoding/json"
	"fmt"
)

// AdapterName represents adapter names in spec 02, section 4.1. Registry may register additional implementations.
type AdapterName string

const (
	AdapterCodex      AdapterName = "codex"
	AdapterClaudeCode AdapterName = "claude_code"
)

func (v AdapterName) Validate() error {
	switch v {
	case AdapterCodex, AdapterClaudeCode:
		return nil
	default:
		return fmt.Errorf("invalid AdapterName %q", v)
	}
}

func (v *AdapterName) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, v)
}

// TaskStatus represents task states in spec 02, section 5.1, including states reserved for later phases.
type TaskStatus string

const (
	TaskReceived        TaskStatus = "received"
	TaskPlanning        TaskStatus = "planning"
	TaskQueued          TaskStatus = "queued"
	TaskRunning         TaskStatus = "running"
	TaskVerifying       TaskStatus = "verifying"
	TaskWaitingApproval TaskStatus = "waiting_approval"
	TaskCompleted       TaskStatus = "completed"
	TaskFailed          TaskStatus = "failed"
	TaskCancelled       TaskStatus = "cancelled"
	TaskTimedOut        TaskStatus = "timed_out"
	TaskBlocked         TaskStatus = "blocked"
)

func (v TaskStatus) Validate() error {
	switch v {
	case TaskReceived, TaskPlanning, TaskQueued, TaskRunning, TaskVerifying, TaskWaitingApproval, TaskCompleted, TaskFailed, TaskCancelled, TaskTimedOut, TaskBlocked:
		return nil
	default:
		return fmt.Errorf("invalid TaskStatus %q", v)
	}
}

func (v *TaskStatus) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, v)
}

// ExecutionStatus represents execution states in spec 02, section 5.2.
type ExecutionStatus string

const (
	ExecutionPending     ExecutionStatus = "pending"
	ExecutionClaimed     ExecutionStatus = "claimed"
	ExecutionPreparing   ExecutionStatus = "preparing"
	ExecutionRunning     ExecutionStatus = "running"
	ExecutionVerifying   ExecutionStatus = "verifying"
	ExecutionPublishing  ExecutionStatus = "publishing"
	ExecutionSucceeded   ExecutionStatus = "succeeded"
	ExecutionBlocked     ExecutionStatus = "blocked"
	ExecutionFailed      ExecutionStatus = "failed"
	ExecutionInterrupted ExecutionStatus = "interrupted"
	ExecutionCancelled   ExecutionStatus = "cancelled"
	ExecutionTimedOut    ExecutionStatus = "timed_out"
)

func (v ExecutionStatus) Validate() error {
	switch v {
	case ExecutionPending, ExecutionClaimed, ExecutionPreparing, ExecutionRunning, ExecutionVerifying, ExecutionPublishing, ExecutionSucceeded, ExecutionBlocked, ExecutionFailed, ExecutionInterrupted, ExecutionCancelled, ExecutionTimedOut:
		return nil
	default:
		return fmt.Errorf("invalid ExecutionStatus %q", v)
	}
}

func (v *ExecutionStatus) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, v)
}

// ExecutionEventType represents execution event types in spec 02, section 5.3.
type ExecutionEventType string

const (
	EventStatusChanged      ExecutionEventType = "status_changed"
	EventLog                ExecutionEventType = "log"
	EventWorktreeReady      ExecutionEventType = "worktree_ready"
	EventContextPinned      ExecutionEventType = "context_pinned"
	EventHarnessStarted     ExecutionEventType = "harness_started"
	EventHarnessFinished    ExecutionEventType = "harness_finished"
	EventGateResult         ExecutionEventType = "gate_result"
	EventBlocked            ExecutionEventType = "blocked"
	EventPushed             ExecutionEventType = "pushed"
	EventPRCreated          ExecutionEventType = "pr_created"
	EventNotificationSent   ExecutionEventType = "notification_sent"
	EventNotificationFailed ExecutionEventType = "notification_failed"
)

func (v ExecutionEventType) Validate() error {
	switch v {
	case EventStatusChanged, EventLog, EventWorktreeReady, EventContextPinned, EventHarnessStarted, EventHarnessFinished, EventGateResult, EventBlocked, EventPushed, EventPRCreated, EventNotificationSent, EventNotificationFailed:
		return nil
	default:
		return fmt.Errorf("invalid ExecutionEventType %q", v)
	}
}

func (v *ExecutionEventType) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, v)
}

// ErrorClass represents error classifications in spec 02, section 5.4.
type ErrorClass string

const (
	ErrorConfig          ErrorClass = "config_error"
	ErrorExecutionFailed ErrorClass = "execution_failed"
	ErrorTestFailed      ErrorClass = "test_failed"
	ErrorTimeout         ErrorClass = "timeout"
	ErrorCancelled       ErrorClass = "cancelled"
	ErrorInterrupted     ErrorClass = "interrupted"
	ErrorPublishFailed   ErrorClass = "publish_failed"
	ErrorSandbox         ErrorClass = "sandbox_error"
)

func (v ErrorClass) Validate() error {
	switch v {
	case ErrorConfig, ErrorExecutionFailed, ErrorTestFailed, ErrorTimeout, ErrorCancelled, ErrorInterrupted, ErrorPublishFailed, ErrorSandbox:
		return nil
	default:
		return fmt.Errorf("invalid ErrorClass %q", v)
	}
}

func (v *ErrorClass) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, v)
}

// BlockCause represents block causes in spec 02, section 5.6.
type BlockCause string

const (
	BlockGate1TestChange   BlockCause = "gate1_test_change"
	BlockGate1Unverifiable BlockCause = "gate1_unverifiable"
	BlockGate2MissingTests BlockCause = "gate2_missing_tests"
	BlockGate2Unverifiable BlockCause = "gate2_unverifiable"
	BlockGate3Mismatch     BlockCause = "gate3_mismatch"
	BlockContextConflict   BlockCause = "context_conflict"
	BlockGitState          BlockCause = "git_state"
)

func (v BlockCause) Validate() error {
	switch v {
	case BlockGate1TestChange, BlockGate1Unverifiable, BlockGate2MissingTests, BlockGate2Unverifiable, BlockGate3Mismatch, BlockContextConflict, BlockGitState:
		return nil
	default:
		return fmt.Errorf("invalid BlockCause %q", v)
	}
}

func (v *BlockCause) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, v)
}

// ResultStatus represents harness result states in spec 02, section 5.8.
type ResultStatus string

const (
	ResultSucceeded ResultStatus = "succeeded"
	ResultFailed    ResultStatus = "failed"
)

func (v ResultStatus) Validate() error {
	switch v {
	case ResultSucceeded, ResultFailed:
		return nil
	default:
		return fmt.Errorf("invalid ResultStatus %q", v)
	}
}

func (v *ResultStatus) UnmarshalJSON(data []byte) error {
	return decodeEnum(data, v)
}

func decodeEnum[T interface {
	~string
	Validate() error
}](data []byte, target *T) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	value := T(text)
	if err := value.Validate(); err != nil {
		return err
	}
	*target = value
	return nil
}
