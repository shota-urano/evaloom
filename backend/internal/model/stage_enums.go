package model

import "fmt"

// Stage follows data-model sections 3–5.
type Stage string

const (
	StagePrepare Stage = "prepare"
	StageWork    Stage = "work"
	StageCheck   Stage = "check"
	StagePublish Stage = "publish"
)

func (v Stage) Validate() error {
	switch v {
	case StagePrepare, StageWork, StageCheck, StagePublish:
		return nil
	default:
		return fmt.Errorf("invalid Stage %q", v)
	}
}

func (v *Stage) UnmarshalJSON(data []byte) error { return decodeEnum(data, v) }

// StageStatus follows data-model sections 3–5.
type StageStatus string

const (
	StageRunning       StageStatus = "running"
	StageSucceeded     StageStatus = "succeeded"
	StageFailed        StageStatus = "failed"
	StageBlocked       StageStatus = "blocked"
	StageNeedsApproval StageStatus = "needs_approval"
	StageUnknown       StageStatus = "unknown"
	StageQuestion      StageStatus = "question"
	StageCancelled     StageStatus = "cancelled"
	StageTimedOut      StageStatus = "timed_out"
)

func (v StageStatus) Validate() error {
	switch v {
	case StageRunning, StageSucceeded, StageFailed, StageBlocked, StageNeedsApproval, StageUnknown, StageQuestion, StageCancelled, StageTimedOut:
		return nil
	default:
		return fmt.Errorf("invalid StageStatus %q", v)
	}
}

func (v *StageStatus) UnmarshalJSON(data []byte) error { return decodeEnum(data, v) }

// ExecutionMode follows data-model sections 3–5.
type ExecutionMode string

const (
	ModeContinue ExecutionMode = "continue"
	ModeFresh    ExecutionMode = "fresh"
)

func (v ExecutionMode) Validate() error {
	switch v {
	case ModeContinue, ModeFresh:
		return nil
	default:
		return fmt.Errorf("invalid ExecutionMode %q", v)
	}
}

func (v *ExecutionMode) UnmarshalJSON(data []byte) error { return decodeEnum(data, v) }

// TaskSource follows data-model sections 3–5.
type TaskSource string

const (
	SourceManual TaskSource = "manual"
	SourceLinear TaskSource = "linear"
)

func (v TaskSource) Validate() error {
	switch v {
	case SourceManual, SourceLinear:
		return nil
	default:
		return fmt.Errorf("invalid TaskSource %q", v)
	}
}

func (v *TaskSource) UnmarshalJSON(data []byte) error { return decodeEnum(data, v) }

// RouteKind follows data-model sections 3–5.
type RouteKind string

const (
	RouteProject RouteKind = "project"
	RouteHarness RouteKind = "harness"
)

func (v RouteKind) Validate() error {
	switch v {
	case RouteProject, RouteHarness:
		return nil
	default:
		return fmt.Errorf("invalid RouteKind %q", v)
	}
}

func (v *RouteKind) UnmarshalJSON(data []byte) error { return decodeEnum(data, v) }

// RouteProposalStatus follows data-model sections 3–5.
type RouteProposalStatus string

const (
	ProposalQueued     RouteProposalStatus = "queued"
	ProposalRunning    RouteProposalStatus = "running"
	ProposalProposed   RouteProposalStatus = "proposed"
	ProposalFailed     RouteProposalStatus = "failed"
	ProposalDecided    RouteProposalStatus = "decided"
	ProposalSuperseded RouteProposalStatus = "superseded"
	ProposalCancelled  RouteProposalStatus = "cancelled"
)

func (v RouteProposalStatus) Validate() error {
	switch v {
	case ProposalQueued, ProposalRunning, ProposalProposed, ProposalFailed, ProposalDecided, ProposalSuperseded, ProposalCancelled:
		return nil
	default:
		return fmt.Errorf("invalid RouteProposalStatus %q", v)
	}
}

func (v *RouteProposalStatus) UnmarshalJSON(data []byte) error { return decodeEnum(data, v) }

// DecisionKind follows data-model sections 3–5.
type DecisionKind string

const (
	DecisionRoute    DecisionKind = "route"
	DecisionApproval DecisionKind = "approval"
	DecisionBlocked  DecisionKind = "blocked"
	DecisionQuestion DecisionKind = "question"
)

func (v DecisionKind) Validate() error {
	switch v {
	case DecisionRoute, DecisionApproval, DecisionBlocked, DecisionQuestion:
		return nil
	default:
		return fmt.Errorf("invalid DecisionKind %q", v)
	}
}

func (v *DecisionKind) UnmarshalJSON(data []byte) error { return decodeEnum(data, v) }

// DecisionCause follows data-model sections 3–5.
type DecisionCause string

const (
	CauseCheckBlocked     DecisionCause = "check_blocked"
	CausePublishUnknown   DecisionCause = "publish_unknown"
	CauseContextConflict  DecisionCause = "context_conflict"
	CauseWorkspaceMissing DecisionCause = "workspace_missing"
	CauseQuestion         DecisionCause = "question"
)

func (v DecisionCause) Validate() error {
	switch v {
	case CauseCheckBlocked, CausePublishUnknown, CauseContextConflict, CauseWorkspaceMissing, CauseQuestion:
		return nil
	default:
		return fmt.Errorf("invalid DecisionCause %q", v)
	}
}

func (v *DecisionCause) UnmarshalJSON(data []byte) error { return decodeEnum(data, v) }

// IntakeBlockCause follows data-model sections 3–5.
type IntakeBlockCause string

const (
	IntakeMissingAcceptanceCriteria IntakeBlockCause = "missing_acceptance_criteria"
	IntakeConfigMissingProfile      IntakeBlockCause = "config_missing_profile"
)

func (v IntakeBlockCause) Validate() error {
	switch v {
	case IntakeMissingAcceptanceCriteria, IntakeConfigMissingProfile:
		return nil
	default:
		return fmt.Errorf("invalid IntakeBlockCause %q", v)
	}
}

func (v *IntakeBlockCause) UnmarshalJSON(data []byte) error { return decodeEnum(data, v) }

// ValidateSpecialHarness rejects classifications reserved for Evaloom
// (harness contract section 13). The caller still checks stage/status context.
func (v ErrorClass) ValidateSpecialHarness() error {
	switch v {
	case ErrorConfig, ErrorPrepareFailed, ErrorTestFailed, ErrorPublishFailed, ErrorPublishUnknown, ErrorCancelled:
		return nil
	default:
		return fmt.Errorf("errorClass %q cannot be written by a special harness", v)
	}
}

// LinearOutboxAction follows data-model section 5.8.
type LinearOutboxAction string

const (
	OutboxComment  LinearOutboxAction = "comment"
	OutboxSetState LinearOutboxAction = "set_state"
	OutboxAddLabel LinearOutboxAction = "add_label"
)

func (v LinearOutboxAction) Validate() error {
	switch v {
	case OutboxComment, OutboxSetState, OutboxAddLabel:
		return nil
	default:
		return fmt.Errorf("invalid LinearOutboxAction %q", v)
	}
}
func (v *LinearOutboxAction) UnmarshalJSON(data []byte) error { return decodeEnum(data, v) }

// LinearOutboxKind follows data-model section 5.8.
type LinearOutboxKind string

const (
	OutboxMissingAcceptanceCriteria LinearOutboxKind = "missing_acceptance_criteria"
	OutboxDecisionOpen              LinearOutboxKind = "decision_open"
	OutboxSucceeded                 LinearOutboxKind = "succeeded"
	OutboxFailed                    LinearOutboxKind = "failed"
	OutboxCancelled                 LinearOutboxKind = "cancelled"
	OutboxStarted                   LinearOutboxKind = "started"
	OutboxWaitingDecision           LinearOutboxKind = "waiting_decision"
	OutboxCompleted                 LinearOutboxKind = "completed"
	OutboxRouteApproved             LinearOutboxKind = "route_approved"
)

func (v LinearOutboxKind) Validate() error {
	switch v {
	case OutboxMissingAcceptanceCriteria, OutboxDecisionOpen, OutboxSucceeded, OutboxFailed, OutboxCancelled, OutboxStarted, OutboxWaitingDecision, OutboxCompleted, OutboxRouteApproved:
		return nil
	default:
		return fmt.Errorf("invalid LinearOutboxKind %q", v)
	}
}
func (v *LinearOutboxKind) UnmarshalJSON(data []byte) error { return decodeEnum(data, v) }

// ValidateForAction checks the pairs in data-model section 5.8.
func (v LinearOutboxKind) ValidateForAction(action LinearOutboxAction) error {
	switch action {
	case OutboxComment:
		switch v {
		case OutboxMissingAcceptanceCriteria, OutboxDecisionOpen, OutboxSucceeded, OutboxFailed, OutboxCancelled:
			return nil
		}
	case OutboxSetState:
		switch v {
		case OutboxStarted, OutboxWaitingDecision, OutboxCompleted, OutboxCancelled:
			return nil
		}
	case OutboxAddLabel:
		if v == OutboxRouteApproved {
			return nil
		}
	}
	return fmt.Errorf("invalid outbox action/kind %q/%q", action, v)
}
