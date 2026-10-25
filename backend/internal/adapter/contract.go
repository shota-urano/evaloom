package adapter

import (
	"encoding/json"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// HarnessRequest is the adapter input described in spec 02, section 5.7.
// JSON decoding validates the version, required fields and field types.
type HarnessRequest struct {
	ContractVersion string         `json:"contractVersion"`
	ExecutionID     string         `json:"executionId"`
	Task            RequestTask    `json:"task"`
	Project         RequestProject `json:"project"`
	Profile         RequestProfile `json:"profile"`
	Context         RequestContext `json:"context"`
	Limits          RequestLimits  `json:"limits"`
}

type RequestTask struct {
	ID                 string `json:"id"`
	Title              string `json:"title"`
	Body               string `json:"body"`
	AcceptanceCriteria string `json:"acceptanceCriteria"`
}

type RequestProject struct {
	ID      string `json:"id"`
	Workdir string `json:"workdir"`
	Branch  string `json:"branch"`
	BaseSHA string `json:"baseSha"`
}

type RequestProfile struct {
	HarnessID  string          `json:"harnessId"`
	Version    string          `json:"version"`
	CommitSHA  string          `json:"commitSha"`
	ModelID    string          `json:"modelId"`
	Definition json.RawMessage `json:"definition"`
}

type RequestContext struct {
	Instructions string   `json:"instructions"`
	RevisionIDs  []string `json:"revisionIds"`
}

type RequestLimits struct {
	TimeoutSec int `json:"timeoutSec"`
}

// HarnessResult is the output described in spec 02, section 5.8. Runner fills
// HeadSHA, Commits and DiffArtifactID from git; Git Publisher supplies PRURL.
type HarnessResult struct {
	ContractVersion string       `json:"contractVersion"`
	Status          ResultStatus `json:"status"`
	ErrorClass      *ErrorClass  `json:"errorClass"`
	SessionID       SessionID    `json:"sessionId"`
	HeadSHA         string       `json:"headSha"`
	Commits         []string     `json:"commits"`
	DiffArtifactID  string       `json:"diffArtifactId"`
	Summary         string       `json:"summary"`
	LogArtifactIDs  []string     `json:"logArtifactIds"`
	ArtifactIDs     []string     `json:"artifactIds"`
	Usage           Usage        `json:"usage"`
	PRURL           *string      `json:"prUrl"`
}

// Aliases preserve the adapter API while sharing validated enum types.
type ResultStatus = model.ResultStatus

const (
	ResultSucceeded = model.ResultSucceeded
	ResultFailed    = model.ResultFailed
)

type Usage struct {
	InputTokens  *int64 `json:"inputTokens"`
	OutputTokens *int64 `json:"outputTokens"`
	WallTimeMS   int64  `json:"wallTimeMs"`
}

// ErrorClass follows spec 02, section 5.4. Adapters use config_error,
// execution_failed, timeout and cancelled; the remaining classes belong to Runner.
type ErrorClass = model.ErrorClass

const (
	ErrorConfig          = model.ErrorConfig
	ErrorExecutionFailed = model.ErrorExecutionFailed
	ErrorTestFailed      = model.ErrorTestFailed
	ErrorTimeout         = model.ErrorTimeout
	ErrorCancelled       = model.ErrorCancelled
	ErrorInterrupted     = model.ErrorInterrupted
	ErrorPublishFailed   = model.ErrorPublishFailed
	ErrorSandbox         = model.ErrorSandbox
)
