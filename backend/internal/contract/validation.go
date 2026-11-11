package contract

import (
	"encoding/json"
	"fmt"
	"math"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

var operationPattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
var outputKindPattern = regexp.MustCompile(`^[a-z_]+$`)

func contentID(value string) error {
	if len(value) < 1 || len(value) > 256 {
		return fmt.Errorf("contentId: expected 1–256 printable ASCII characters")
	}
	for _, c := range []byte(value) {
		if c < 0x20 || c > 0x7e {
			return fmt.Errorf("contentId: expected printable ASCII")
		}
	}
	return nil
}

func optionalContentID(value *string) error {
	if value != nil {
		return contentID(*value)
	}
	return nil
}

func boundedText(value string, limit int, name string) error {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > limit {
		return fmt.Errorf("%s: expected at most %d characters", name, limit)
	}
	return nil
}

func relativePath(value string) bool {
	clean := path.Clean(value)
	return value != "" && clean != ".." && !path.IsAbs(value) &&
		!strings.HasPrefix(clean, "../") && !strings.ContainsAny(value, "\\\x00")
}

func (r Request) Validate() error {
	if r.ContractVersion != Version {
		return fmt.Errorf("contractVersion: expected %q", Version)
	}
	if err := r.Stage.Validate(); err != nil {
		return err
	}
	if err := r.Execution.Mode.Validate(); err != nil {
		return err
	}
	if r.Execution.Seq < 1 {
		return fmt.Errorf("execution.seq: must be positive")
	}
	if s := r.Execution.ResumeStage; s != nil && *s != model.StageWork && *s != model.StagePublish {
		return fmt.Errorf("execution.resumeStage: expected work or publish")
	}
	if err := r.Task.Source.Kind.Validate(); err != nil {
		return err
	}
	if r.Task.Source.Kind == model.SourceManual && (r.Task.Source.Identifier != nil || r.Task.Source.URL != nil) {
		return fmt.Errorf("task.source: manual source requires null identifier and url")
	}
	if r.Task.Source.Kind == model.SourceLinear && (r.Task.Source.Identifier == nil || r.Task.Source.URL == nil) {
		return fmt.Errorf("task.source: linear source requires identifier and url")
	}
	if err := r.Route.Kind.Validate(); err != nil {
		return err
	}
	if r.Route.Kind == model.RouteHarness && (r.Route.ProjectID != nil || r.Route.ProjectName != nil) {
		return fmt.Errorf("route: harness route requires null projectId and projectName")
	}
	if r.Route.Kind == model.RouteProject && (r.Route.ProjectID == nil || r.Route.ProjectName == nil) {
		return fmt.Errorf("route: project route requires projectId and projectName")
	}
	if err := objectJSON(r.Params, "params"); err != nil {
		return err
	}
	if r.Route.Kind == model.RouteHarness {
		fields, _ := objectFields(r.Params, "params")
		if len(fields) != 0 {
			return fmt.Errorf("params: harness route requires an empty object")
		}
	}
	if err := optionalContentID(r.CheckedContentID); err != nil {
		return err
	}
	if r.Stage != model.StagePublish && (r.CheckedContentID != nil || len(r.Approvals) > 0) {
		return fmt.Errorf("checkedContentId and approvals: only available in publish")
	}
	if r.Stage == model.StagePublish && r.CheckedContentID == nil {
		return fmt.Errorf("checkedContentId: required in publish")
	}
	for _, a := range r.Approvals {
		if !operationPattern.MatchString(a.Operation) {
			return fmt.Errorf("approvals.operation: invalid name")
		}
		switch a.Status {
		case "approved":
			if a.ID == nil || *a.ID == "" || a.ContentID == nil {
				return fmt.Errorf("approvals: approved requires id and contentId")
			}
		case "waived":
			if a.ID != nil {
				return fmt.Errorf("approvals: waived requires null id")
			}
		default:
			return fmt.Errorf("approvals.status: expected approved or waived")
		}
		if err := optionalContentID(a.ContentID); err != nil {
			return err
		}
	}
	if r.Answer != nil && r.Stage != model.StageWork {
		return fmt.Errorf("answer: only available in work")
	}
	if r.Previous != nil {
		if err := optionalContentID(r.Previous.CheckedContentID); err != nil {
			return err
		}
		if err := validateOutputs(r.Previous.Outputs); err != nil {
			return err
		}
	}
	if r.Limits.TimeoutSec < 1 {
		return fmt.Errorf("limits.timeoutSec: must be positive")
	}
	return nil
}

// ParamsValidator checks draft 2020-12 using the schema from the pinned
// repository. Loading that file and resolving its references is Registry's job.
type ParamsValidator interface {
	ValidateParams(schemaPath string, params json.RawMessage) error
}

func (r Request) ValidateForProfile(profile ProfileDefinition, schemas ParamsValidator) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if profile.AcceptanceCriteria == "required" && strings.TrimSpace(r.Task.AcceptanceCriteria) == "" {
		return fmt.Errorf("task.acceptanceCriteria: required by profile")
	}
	if profile.Params == nil || profile.Params.Schema == "" {
		fields, _ := objectFields(r.Params, "params")
		if len(fields) != 0 {
			return fmt.Errorf("params: profile without schema only accepts an empty object")
		}
		return nil
	}
	if schemas == nil {
		return fmt.Errorf("params: schema validator is required")
	}
	return schemas.ValidateParams(profile.Params.Schema, r.Params)
}

func (r Result) Validate() error {
	if r.ContractVersion != Version {
		return fmt.Errorf("contractVersion: expected %q", Version)
	}
	if err := r.Stage.Validate(); err != nil {
		return err
	}
	if r.Stage == model.StageWork {
		return fmt.Errorf("stage: work does not write result JSON")
	}
	switch r.Status {
	case model.StageSucceeded, model.StageFailed:
	case model.StageBlocked:
		if r.Stage != model.StageCheck {
			return fmt.Errorf("status: blocked requires check")
		}
	case model.StageNeedsApproval:
		if r.Stage != model.StageCheck && r.Stage != model.StagePublish {
			return fmt.Errorf("status: needs_approval requires check or publish")
		}
	case model.StageUnknown:
		if r.Stage != model.StagePublish || r.ErrorClass == nil || *r.ErrorClass != model.ErrorPublishUnknown {
			return fmt.Errorf("status: unknown requires publish and publish_unknown")
		}
	default:
		return fmt.Errorf("status: invalid program result %q", r.Status)
	}
	if r.ErrorClass != nil {
		if err := r.ErrorClass.ValidateSpecialHarness(); err != nil {
			return err
		}
	}
	if (r.Status == model.StageFailed || r.Status == model.StageUnknown) && r.ErrorClass == nil {
		return fmt.Errorf("errorClass: required for failed or unknown")
	}
	if err := boundedText(r.Message, 1000, "message"); err != nil {
		return err
	}
	if err := optionalContentID(r.ContentID); err != nil {
		return err
	}
	if r.Stage == model.StageCheck && (r.Status == model.StageSucceeded || r.Status == model.StageNeedsApproval || r.Status == model.StageBlocked) && r.ContentID == nil {
		return fmt.Errorf("contentId: required for successful or blocked check")
	}
	if r.Stage == model.StagePrepare && (r.ContentID != nil || r.Summary != nil || len(r.Findings) > 0 || len(r.ApprovalRequests) > 0 || len(r.Outputs) > 0) {
		return fmt.Errorf("prepare: check/publish result data is forbidden")
	}
	if r.Summary != nil {
		if err := boundedText(*r.Summary, 4000, "summary"); err != nil {
			return err
		}
	}
	if len(r.Findings) > 500 || (r.Stage != model.StageCheck && len(r.Findings) > 0) {
		return fmt.Errorf("findings: only check may report up to 500 findings")
	}
	for _, finding := range r.Findings {
		if err := finding.Validate(); err != nil {
			return err
		}
	}
	for _, a := range r.ApprovalRequests {
		if !operationPattern.MatchString(a.Operation) {
			return fmt.Errorf("approvalRequests.operation: invalid name")
		}
		if err := contentID(a.ContentID); err != nil {
			return err
		}
		if err := objectJSON(a.Target, "approvalRequests.target"); err != nil {
			return err
		}
		// Summary's length is only specified for the result's summary, not
		// for an approval request. Do not invent a second limit here.
	}
	if r.Stage != model.StagePublish && len(r.Outputs) > 0 {
		return fmt.Errorf("outputs: only publish may report outputs")
	}
	if err := validateOutputs(r.Outputs); err != nil {
		return err
	}
	if r.Stage != model.StagePrepare && len(r.ReportedContext) > 0 {
		return fmt.Errorf("reportedContext: only prepare may report context")
	}
	if r.Usage != nil && ((r.Usage.InputTokens != nil && *r.Usage.InputTokens < 0) ||
		(r.Usage.OutputTokens != nil && *r.Usage.OutputTokens < 0) ||
		(r.Usage.CostAmount != nil && (*r.Usage.CostAmount < 0 || math.IsNaN(*r.Usage.CostAmount) || math.IsInf(*r.Usage.CostAmount, 0)))) {
		return fmt.Errorf("usage: expected nonnegative finite amounts")
	}
	return nil
}

// ValidateForProfile binds the result to the stage that actually ran and the
// pinned declaration. JSON decoding alone cannot know either of these values.
func (r Result) ValidateForProfile(stage model.Stage, profile ProfileDefinition) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Stage != stage {
		return fmt.Errorf("stage: differs from the invoked stage")
	}
	declared := map[string]bool{}
	for _, a := range profile.Approvals {
		declared[a.Operation] = true
	}
	for _, a := range r.ApprovalRequests {
		if !declared[a.Operation] {
			return fmt.Errorf("approvalRequests.operation: %q is not declared", a.Operation)
		}
	}
	return nil
}

func (f Finding) Validate() error {
	if !operationPattern.MatchString(f.Rule) {
		return fmt.Errorf("finding.rule: invalid name")
	}
	if f.Severity != "error" && f.Severity != "warning" && f.Severity != "info" {
		return fmt.Errorf("finding.severity: expected error, warning or info")
	}
	if err := boundedText(f.Message, 1000, "finding.message"); err != nil {
		return err
	}
	if f.Location != nil && (!relativePath(f.Location.Path) || f.Location.Line < 0) {
		return fmt.Errorf("finding.location: expected relative path and nonnegative line")
	}
	return nil
}

func validateOutputs(outputs []Output) error {
	for _, output := range outputs {
		if !outputKindPattern.MatchString(output.Kind) {
			return fmt.Errorf("outputs.kind: expected lowercase letters or underscore")
		}
	}
	return nil
}
