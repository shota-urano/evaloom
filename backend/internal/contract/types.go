// Package contract defines the special-harness v1 wire format. It is separate
// from the legacy micro-harness adapter protocol; no development-specific data
// belongs in these messages. See docs/contract/harness-contract-v1.md.
package contract

import (
	"encoding/json"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

const Version = "1"

type Request struct {
	ContractVersion  string             `json:"contractVersion" required:"true"`
	Stage            model.Stage        `json:"stage" required:"true"`
	Execution        RequestExecution   `json:"execution" required:"true"`
	Task             RequestTask        `json:"task" required:"true"`
	Route            RequestRoute       `json:"route" required:"true"`
	Params           json.RawMessage    `json:"params" required:"true"`
	Profile          RequestProfile     `json:"profile" required:"true"`
	Paths            RequestPaths       `json:"paths" required:"true"`
	Previous         *PreviousExecution `json:"previous" required:"true" nullable:"true"`
	CheckedContentID *string            `json:"checkedContentId,omitempty" nullable:"true"`
	Approvals        []RequestApproval  `json:"approvals,omitempty"`
	Answer           *Answer            `json:"answer" required:"true" nullable:"true"`
	Limits           RequestLimits      `json:"limits" required:"true"`
}

type RequestExecution struct {
	ID          string              `json:"id" required:"true"`
	Seq         int                 `json:"seq" required:"true"`
	Mode        model.ExecutionMode `json:"mode" required:"true"`
	ResumeStage *model.Stage        `json:"resumeStage" required:"true" nullable:"true"`
}

type RequestTask struct {
	ID                 string        `json:"id" required:"true"`
	Title              string        `json:"title" required:"true"`
	Body               string        `json:"body" required:"true"`
	AcceptanceCriteria string        `json:"acceptanceCriteria" required:"true"`
	Source             RequestSource `json:"source" required:"true"`
}

type RequestSource struct {
	Kind       model.TaskSource `json:"kind" required:"true"`
	Identifier *string          `json:"identifier" required:"true" nullable:"true"`
	URL        *string          `json:"url" required:"true" nullable:"true"`
}

type RequestRoute struct {
	Kind        model.RouteKind `json:"kind" required:"true"`
	ProjectID   *string         `json:"projectId" required:"true" nullable:"true"`
	ProjectName *string         `json:"projectName" required:"true" nullable:"true"`
}

type RequestProfile struct {
	HarnessID string `json:"harnessId" required:"true"`
	Version   string `json:"version" required:"true"`
	CommitSHA string `json:"commitSha" required:"true"`
}

type RequestPaths struct {
	Profile   string `json:"profile" required:"true"`
	Workspace string `json:"workspace" required:"true"`
	Out       string `json:"out" required:"true"`
	Inputs    string `json:"inputs" required:"true"`
	Records   string `json:"records" required:"true"`
	Scratch   string `json:"scratch" required:"true"`
}

type PreviousExecution struct {
	ExecutionID      string   `json:"executionId" required:"true"`
	CheckedContentID *string  `json:"checkedContentId" required:"true" nullable:"true"`
	Outputs          []Output `json:"outputs" required:"true"`
}

type RequestApproval struct {
	ID        *string `json:"id" required:"true" nullable:"true"`
	Operation string  `json:"operation" required:"true"`
	ContentID *string `json:"contentId" required:"true" nullable:"true"`
	Status    string  `json:"status" required:"true"`
}

type Answer struct {
	DecisionID string  `json:"decisionId" required:"true"`
	Text       string  `json:"text" required:"true"`
	Option     *string `json:"option" required:"true" nullable:"true"`
}

type RequestLimits struct {
	TimeoutSec int `json:"timeoutSec" required:"true"`
}

// Result is written by program stages, never by the work micro-harness.
// Fields belonging to another stage can be absent, null (nullable scalars), or
// empty arrays as in the contract's example, but cannot carry data.
type Result struct {
	ContractVersion  string            `json:"contractVersion" required:"true"`
	Stage            model.Stage       `json:"stage" required:"true"`
	Status           model.StageStatus `json:"status" required:"true"`
	ErrorClass       *model.ErrorClass `json:"errorClass" nullable:"true"`
	Message          string            `json:"message" required:"true"`
	ContentID        *string           `json:"contentId,omitempty" nullable:"true"`
	Summary          *string           `json:"summary,omitempty" nullable:"true"`
	Findings         []Finding         `json:"findings,omitempty"`
	ApprovalRequests []ApprovalRequest `json:"approvalRequests,omitempty"`
	Outputs          []Output          `json:"outputs,omitempty"`
	ReportedContext  []ReportedContext `json:"reportedContext,omitempty"`
	Usage            *Usage            `json:"usage" nullable:"true"`
}

type ApprovalRequest struct {
	Operation string          `json:"operation" required:"true"`
	ContentID string          `json:"contentId" required:"true"`
	Target    json.RawMessage `json:"target" required:"true"`
	Summary   string          `json:"summary" required:"true"`
}

type Output struct {
	Kind  string `json:"kind" required:"true"`
	Label string `json:"label" required:"true"`
	URL   string `json:"url" required:"true"`
}

type ReportedContext struct {
	Source   string `json:"source" required:"true"`
	Path     string `json:"path" required:"true"`
	Revision string `json:"revision" required:"true"`
	SHA256   string `json:"sha256" required:"true"`
}

type Usage struct {
	InputTokens  *int64   `json:"inputTokens" required:"true" nullable:"true"`
	OutputTokens *int64   `json:"outputTokens" required:"true" nullable:"true"`
	CostAmount   *float64 `json:"costAmount" required:"true" nullable:"true"`
}

type Finding struct {
	Rule     string           `json:"rule" required:"true"`
	Severity string           `json:"severity" required:"true"`
	Message  string           `json:"message" required:"true"`
	Location *FindingLocation `json:"location,omitempty"`
}

type FindingLocation struct {
	Path string `json:"path" required:"true"`
	Line int    `json:"line" required:"true"`
}

type ProfileDefinition struct {
	ContractVersion    string                `json:"contractVersion" required:"true"`
	ProfileVersion     string                `json:"profileVersion" required:"true"`
	Name               string                `json:"name" required:"true"`
	Description        string                `json:"description" required:"true"`
	AcceptanceCriteria string                `json:"acceptanceCriteria" required:"true"`
	Params             *ProfileParams        `json:"params,omitempty"`
	Stages             ProfileStages         `json:"stages" required:"true"`
	Approvals          []ApprovalDeclaration `json:"approvals,omitempty"`
	Records            *ProfileRecords       `json:"records,omitempty"`
}

type ProfileParams struct {
	Schema string `json:"schema,omitempty"`
}

type ProfileStages struct {
	Prepare *ProgramStage `json:"prepare,omitempty"`
	Work    WorkStage     `json:"work" required:"true"`
	Check   ProgramStage  `json:"check" required:"true"`
	Publish ProgramStage  `json:"publish" required:"true"`
}

type ProgramStage struct {
	Command    []string `json:"command" required:"true"`
	TimeoutSec int      `json:"timeoutSec" required:"true"`
	Network    *Network `json:"network,omitempty"`
	Secrets    []Secret `json:"secrets,omitempty"`
	Runtime    *Runtime `json:"runtime,omitempty"`
}

type Network struct {
	Allow []string `json:"allow" required:"true"`
}

type Secret struct {
	Slot   string `json:"slot" required:"true"`
	Access string `json:"access" required:"true"`
}

type Runtime struct {
	Kind  string `json:"kind" required:"true"`
	Image string `json:"image" required:"true"`
}

type WorkStage struct {
	MicroHarness    model.AdapterName `json:"microHarness" required:"true"`
	Model           string            `json:"model" required:"true"`
	Instructions    []string          `json:"instructions,omitempty"`
	Skills          []string          `json:"skills,omitempty"`
	Settings        []string          `json:"settings,omitempty"`
	ToolPermissions json.RawMessage   `json:"toolPermissions,omitempty"`
	Resume          string            `json:"resume,omitempty"`
	TimeoutSec      int               `json:"timeoutSec" required:"true"`
}

type ApprovalDeclaration struct {
	Operation   string `json:"operation" required:"true"`
	Description string `json:"description" required:"true"`
	Waivable    bool   `json:"waivable,omitempty"`
}

type ProfileRecords struct {
	Keep []string    `json:"keep" required:"true"`
	Pass RecordsPass `json:"pass" required:"true"`
}

type RecordsPass struct {
	Recent int `json:"recent" required:"true"`
}
