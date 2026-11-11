package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/task"
)

// StageReportInvalid is a report that refers to data the Execution does not
// have (an artifact of another Execution or kind). The handler answers 400.
type StageReportInvalid struct{ Field, Reason string }

func (e *StageReportInvalid) Error() string { return e.Field + ": " + e.Reason }

// stageStatuses maps a stage to the Execution state while it runs (spec 02 5.2).
var stageStatuses = map[model.Stage]model.ExecutionStatus{
	model.StagePrepare: model.ExecutionPreparing,
	model.StageWork:    model.ExecutionRunning,
	model.StageCheck:   model.ExecutionVerifying,
	model.StagePublish: model.ExecutionPublishing,
}

// nextStages is the order of the stages (spec 02 5.2, 4.1).
var nextStages = map[model.Stage]model.Stage{
	model.StagePrepare: model.StageWork,
	model.StageWork:    model.StageCheck,
	model.StageCheck:   model.StagePublish,
}

// RunnerErrors are the values of stage_finished.runnerError (spec 04 3.4.1).
// workspace_missing is a decision cause, not an error class.
var RunnerErrors = []string{
	string(model.ErrorConfig), string(model.ErrorSandbox), string(model.ErrorTimeout), string(model.ErrorCancelled),
	string(model.ErrorContract), string(model.ErrorExecutionFailed), "workspace_missing",
}

type reportExecution struct {
	id, taskID, runnerID   string
	status                 model.ExecutionStatus
	taskStatus             model.TaskStatus
	resumeStage, profileID sql.NullString
	routeKind, projectID   sql.NullString
	harnessID              sql.NullString
	latest                 bool
}

func readReportExecution(ctx context.Context, tx *sql.Tx, id string) (*reportExecution, error) {
	e := &reportExecution{id: id}
	err := tx.QueryRowContext(ctx, `SELECT e.task_id, COALESCE(e.runner_id, ''), e.status, t.status, e.resume_stage,
		e.profile_id, e.route_kind, e.project_id, e.harness_id,
		e.seq = (SELECT max(seq) FROM executions WHERE task_id = e.task_id)
		FROM executions e JOIN tasks t ON t.id = e.task_id WHERE e.id = ?`, id).
		Scan(&e.taskID, &e.runnerID, &e.status, &e.taskStatus, &e.resumeStage, &e.profileID,
			&e.routeKind, &e.projectID, &e.harnessID, &e.latest)
	return e, err
}

// reportProfile is the part of the pinned Profile definition the reports use.
// Registration validated the whole definition.
type reportProfile struct {
	Stages struct {
		Prepare json.RawMessage `json:"prepare"`
	} `json:"stages"`
	Approvals []contract.ApprovalDeclaration `json:"approvals"`
	Records   *struct {
		Keep []string `json:"keep"`
	} `json:"records"`
}

func (p reportProfile) hasPrepare() bool {
	return len(p.Stages.Prepare) > 0 && string(p.Stages.Prepare) != "null"
}

func readReportProfile(ctx context.Context, tx *sql.Tx, e *reportExecution) (reportProfile, error) {
	var profile reportProfile
	if !e.profileID.Valid {
		return profile, fmt.Errorf("store: Execution %s has no pinned Profile", e.id)
	}
	var definition string
	if err := tx.QueryRowContext(ctx, `SELECT definition FROM harness_profiles WHERE id = ?`, e.profileID.String).Scan(&definition); err != nil {
		return profile, err
	}
	if err := json.Unmarshal([]byte(definition), &profile); err != nil {
		return profile, fmt.Errorf("store: Profile %s definition cannot be read", e.profileID.String)
	}
	return profile, nil
}

func conflictf(format string, args ...any) error {
	return &task.ConflictError{Code: "conflict", Message: fmt.Sprintf(format, args...)}
}

// move changes the Execution and its Task in one step and records the
// status_changed event (spec 02 5.2, 5.3).
func (e *reportExecution) move(ctx context.Context, tx *sql.Tx, to model.ExecutionStatus, resume model.Stage, reason string) error {
	if err := task.TransitionExecutionTx(ctx, tx, task.Transition{
		TaskID: e.taskID, ExecutionID: e.id, FromTask: e.taskStatus,
		FromExecution: e.status, ToExecution: to, ResumeStage: resume,
	}); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"from": string(e.status), "to": string(to), "reason": reason})
	if err != nil {
		return err
	}
	if _, err := AppendExecutionEvent(ctx, tx, e.id, model.EventStatusChanged, payload); err != nil {
		return err
	}
	e.status = to
	e.taskStatus, err = task.TaskStatusForExecution(to)
	return err
}

func appendEvent(ctx context.Context, tx *sql.Tx, executionID string, kind model.ExecutionEventType, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = AppendExecutionEvent(ctx, tx, executionID, kind, data)
	return err
}

func artifactExists(ctx context.Context, tx *sql.Tx, id, executionID, kind string, stage model.Stage) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM artifacts WHERE id = ? AND execution_id = ? AND kind = ? AND stage = ?`,
		id, executionID, kind, stage).Scan(&n)
	return n == 1, err
}

// StageStart is the stage_started report (spec 04 3.4.1).
type StageStart struct {
	ExecutionID, RunnerID string
	Stage                 model.Stage
	RequestArtifactID     string
}

// StartStageTx records the start of a stage and returns its attempt. A resent
// report (the same requestArtifactId) returns the saved attempt and writes
// nothing. The caller owns the transaction and must roll back on any error.
func StartStageTx(ctx context.Context, tx *sql.Tx, in StageStart) (int, error) {
	e, err := readReportExecution(ctx, tx, in.ExecutionID)
	if err != nil {
		return 0, err
	}
	var savedStage string
	var savedAttempt int
	err = tx.QueryRowContext(ctx, `SELECT stage, attempt FROM stage_runs WHERE execution_id = ? AND request_artifact_id = ?`,
		e.id, in.RequestArtifactID).Scan(&savedStage, &savedAttempt)
	if err == nil {
		if savedStage != string(in.Stage) {
			return 0, conflictf("request artifact already started stage %s", savedStage)
		}
		return savedAttempt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if !e.latest {
		return 0, conflictf("execution is not the latest execution")
	}
	if ok, err := artifactExists(ctx, tx, in.RequestArtifactID, e.id, "request_json", in.Stage); err != nil {
		return 0, err
	} else if !ok {
		return 0, &StageReportInvalid{Field: "requestArtifactId", Reason: "not_found"}
	}
	profile, err := readReportProfile(ctx, tx, e)
	if err != nil {
		return 0, err
	}
	switch e.status {
	case model.ExecutionClaimed:
		switch {
		case e.resumeStage.Valid:
			if string(in.Stage) != e.resumeStage.String {
				return 0, conflictf("execution resumes at %s, not %s", e.resumeStage.String, in.Stage)
			}
		case in.Stage == model.StagePrepare && !profile.hasPrepare():
			return 0, conflictf("the pinned Profile declares no prepare stage")
		case in.Stage == model.StageWork && profile.hasPrepare():
			return 0, conflictf("the pinned Profile starts with the prepare stage")
		case in.Stage != model.StagePrepare && in.Stage != model.StageWork:
			return 0, conflictf("execution cannot start at %s", in.Stage)
		}
	case model.ExecutionPreparing, model.ExecutionRunning, model.ExecutionVerifying:
		var current model.Stage
		for stage, status := range stageStatuses {
			if status == e.status {
				current = stage
			}
		}
		if nextStages[current] != in.Stage {
			return 0, conflictf("stage %s does not follow %s", in.Stage, current)
		}
		// The previous stage must have finished and answered next=<stage>;
		// every other outcome moved the Execution out of this state.
		var running int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM stage_runs WHERE execution_id = ? AND stage = ? AND status = 'running'`,
			e.id, current).Scan(&running); err != nil {
			return 0, err
		}
		if running > 0 {
			return 0, conflictf("stage %s has not finished", current)
		}
	default:
		return 0, conflictf("execution %s cannot start a stage", e.status)
	}
	var attempt int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(attempt), 0) + 1 FROM stage_runs WHERE execution_id = ? AND stage = ?`,
		e.id, in.Stage).Scan(&attempt); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO stage_runs (id, execution_id, stage, attempt, status, request_artifact_id, started_at)
		VALUES (?, ?, ?, ?, 'running', ?, ?)`, ulid.Make().String(), e.id, in.Stage, attempt, in.RequestArtifactID,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return 0, err
	}
	from := e.status
	if err := e.move(ctx, tx, stageStatuses[in.Stage], "", "stage_started"); err != nil {
		return 0, err
	}
	if err := appendEvent(ctx, tx, e.id, model.EventStageStarted, map[string]any{"stage": in.Stage, "attempt": attempt}); err != nil {
		return 0, err
	}
	_, err = RecordAuditLog(ctx, tx, AuditLog{
		Actor: "runner:" + in.RunnerID, Action: "execution.result", TargetType: "execution", TargetID: e.id,
		Detail: map[string]any{"taskId": e.taskID, "type": "stage_started", "stage": in.Stage, "attempt": attempt,
			"from": from, "to": e.status},
	})
	return attempt, err
}

// StageFinish is the stage_finished report (spec 04 3.4.1). Result is nil
// when the report's result is null. ReportSHA256 identifies the report's
// content for resends.
type StageFinish struct {
	ExecutionID, RunnerID string
	Stage                 model.Stage
	Attempt               int
	ExitCode              *int
	RunnerError           string
	Result                json.RawMessage
	ResultArtifactID      *string
	Work                  *StageWork
	Usage                 StageUsage
	ReportSHA256          string
}

type StageWork struct {
	SessionID, SummaryArtifactID *string
	Question                     json.RawMessage
}

type StageUsage struct {
	WallTimeMS                int64
	InputTokens, OutputTokens *int64
	CostAmount                *float64
}

// StageNext is the response to stage_finished. CheckedContentID and
// Approvals (the waived operations) accompany next=publish.
type StageNext struct {
	Next             string                     `json:"next"`
	CheckedContentID *string                    `json:"checkedContentId,omitempty"`
	Approvals        []contract.RequestApproval `json:"approvals,omitempty"`
}

const stageStop = "stop"

// MarshalJSON always writes checkedContentId and approvals with next=publish
// (spec 04 3.4.1 step 4), and omits them otherwise.
func (n StageNext) MarshalJSON() ([]byte, error) {
	if n.Next != string(model.StagePublish) {
		return json.Marshal(struct {
			Next string `json:"next"`
		}{n.Next})
	}
	return json.Marshal(struct {
		Next             string                     `json:"next"`
		CheckedContentID *string                    `json:"checkedContentId"`
		Approvals        []contract.RequestApproval `json:"approvals"`
	}{n.Next, n.CheckedContentID, nonNil(n.Approvals)})
}

// workQuestion is out/question.json (contract 8).
type workQuestion struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

func validQuestion(raw json.RawMessage) bool {
	var q workQuestion
	if err := strictJSON(raw, &q); err != nil {
		return false
	}
	if q.Question == "" || utf8.RuneCountInString(q.Question) > 4000 || len(q.Options) > 10 {
		return false
	}
	return true
}

func strictJSON(raw json.RawMessage, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}

// finishOutcome is the stage_runs row and the Execution's next step.
type finishOutcome struct {
	runStatus  model.StageStatus
	errorClass model.ErrorClass
	message    *string
	next       StageNext
}

// FinishStageTx applies a stage_finished report (spec 04 3.4.1). A resend of
// the same report returns the saved response and writes nothing; a different
// report for a finished (stage, attempt) is a conflict. The caller owns the
// transaction and must roll back on any error.
func FinishStageTx(ctx context.Context, tx *sql.Tx, in StageFinish) (StageNext, error) {
	e, err := readReportExecution(ctx, tx, in.ExecutionID)
	if err != nil {
		return StageNext{}, err
	}
	var runID string
	var runStatus model.StageStatus
	err = tx.QueryRowContext(ctx, `SELECT id, status FROM stage_runs WHERE execution_id = ? AND stage = ? AND attempt = ?`,
		e.id, in.Stage, in.Attempt).Scan(&runID, &runStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return StageNext{}, conflictf("stage %s attempt %d was not started", in.Stage, in.Attempt)
	}
	if err != nil {
		return StageNext{}, err
	}
	if runStatus != model.StageRunning {
		return savedFinish(ctx, tx, in)
	}
	if !e.latest || e.status != stageStatuses[in.Stage] {
		return StageNext{}, conflictf("execution %s is not running stage %s", e.status, in.Stage)
	}
	if in.ResultArtifactID != nil {
		if ok, err := artifactExists(ctx, tx, *in.ResultArtifactID, e.id, "result_json", in.Stage); err != nil {
			return StageNext{}, err
		} else if !ok {
			return StageNext{}, &StageReportInvalid{Field: "resultArtifactId", Reason: "not_found"}
		}
	}
	if in.Work != nil && in.Work.SummaryArtifactID != nil {
		if ok, err := artifactExists(ctx, tx, *in.Work.SummaryArtifactID, e.id, "work_summary", in.Stage); err != nil {
			return StageNext{}, err
		} else if !ok {
			return StageNext{}, &StageReportInvalid{Field: "work.summaryArtifactId", Reason: "not_found"}
		}
	}
	if err := RecordUsage(ctx, tx, UsageRecord{
		ID: ulid.Make().String(), ExecutionID: e.id, Stage: string(in.Stage), WallTimeMS: in.Usage.WallTimeMS,
		InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens, CostAmount: in.Usage.CostAmount,
	}); err != nil {
		return StageNext{}, err
	}
	if in.Work != nil && in.Work.SessionID != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE executions SET work_session_id = ? WHERE id = ?`, *in.Work.SessionID, e.id); err != nil {
			return StageNext{}, err
		}
	}
	from := e.status
	out, err := finishStage(ctx, tx, e, in)
	if err != nil {
		return StageNext{}, err
	}
	var errorClass any
	if out.errorClass != "" {
		errorClass = out.errorClass
	}
	if _, err := tx.ExecContext(ctx, `UPDATE stage_runs SET status = ?, exit_code = ?, error_class = ?, result_artifact_id = ?, finished_at = ?
		WHERE id = ? AND status = 'running'`, out.runStatus, in.ExitCode, errorClass, in.ResultArtifactID,
		time.Now().UTC().Format(time.RFC3339Nano), runID); err != nil {
		return StageNext{}, err
	}
	if err := appendEvent(ctx, tx, e.id, model.EventStageFinished, map[string]any{
		"stage": in.Stage, "attempt": in.Attempt, "status": out.runStatus, "errorClass": errorClass, "exitCode": in.ExitCode,
	}); err != nil {
		return StageNext{}, err
	}
	_, err = RecordAuditLog(ctx, tx, AuditLog{
		Actor: "runner:" + in.RunnerID, Action: "execution.result", TargetType: "execution", TargetID: e.id,
		Detail: map[string]any{"taskId": e.taskID, "type": "stage_finished", "stage": in.Stage, "attempt": in.Attempt,
			"from": from, "to": e.status, "reportSha256": in.ReportSHA256, "response": out.next},
	})
	return out.next, err
}

// savedFinish answers a resent stage_finished from its audit record.
func savedFinish(ctx context.Context, tx *sql.Tx, in StageFinish) (StageNext, error) {
	var hash, response sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT json_extract(detail, '$.reportSha256'), json_extract(detail, '$.response')
		FROM audit_logs WHERE action = 'execution.result' AND target_type = 'execution' AND target_id = ?
		AND json_extract(detail, '$.type') = 'stage_finished' AND json_extract(detail, '$.stage') = ?
		AND json_extract(detail, '$.attempt') = ? ORDER BY id DESC LIMIT 1`, in.ExecutionID, in.Stage, in.Attempt).Scan(&hash, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return StageNext{}, conflictf("stage %s attempt %d already ended", in.Stage, in.Attempt)
	}
	if err != nil {
		return StageNext{}, err
	}
	if !hash.Valid || hash.String != in.ReportSHA256 {
		return StageNext{}, conflictf("stage %s attempt %d was reported with different content", in.Stage, in.Attempt)
	}
	var next StageNext
	if err := json.Unmarshal([]byte(response.String), &next); err != nil {
		return StageNext{}, fmt.Errorf("store: saved stage response cannot be read")
	}
	return next, nil
}

func stop() StageNext { return StageNext{Next: stageStop} }

// failExecution ends the Execution with class (spec 04 3.4.1, second row).
func failExecution(ctx context.Context, tx *sql.Tx, e *reportExecution, class model.ErrorClass, message *string) error {
	to := model.ExecutionFailed
	switch class {
	case model.ErrorTimeout:
		to = model.ExecutionTimedOut
	case model.ErrorCancelled:
		to = model.ExecutionCancelled
	}
	if _, err := tx.ExecContext(ctx, `UPDATE executions SET error_class = ?, error_message = COALESCE(?, error_message) WHERE id = ?`,
		class, message, e.id); err != nil {
		return err
	}
	return e.move(ctx, tx, to, "", "stage_finished")
}

func finishStage(ctx context.Context, tx *sql.Tx, e *reportExecution, in StageFinish) (finishOutcome, error) {
	switch in.RunnerError {
	case "":
	case "workspace_missing":
		if e.resumeStage.Valid {
			if err := e.move(ctx, tx, model.ExecutionBlocked, "", "workspace_missing"); err != nil {
				return finishOutcome{}, err
			}
			_, err := openDecision(ctx, tx, e, model.DecisionBlocked, model.CauseWorkspaceMissing, nil, nil, map[string]any{
				"message": "再開に要る作業領域、または前の段の out が Runner にありません。中止して fresh で再実行してください。",
				"stage":   in.Stage,
			})
			return finishOutcome{runStatus: model.StageBlocked, next: stop()}, err
		}
		err := failExecution(ctx, tx, e, model.ErrorConfig, nil)
		return finishOutcome{runStatus: model.StageFailed, errorClass: model.ErrorConfig, next: stop()}, err
	default:
		class := model.ErrorClass(in.RunnerError)
		status := model.StageFailed
		switch class {
		case model.ErrorTimeout:
			status = model.StageTimedOut
		case model.ErrorCancelled:
			status = model.StageCancelled
		}
		err := failExecution(ctx, tx, e, class, nil)
		return finishOutcome{runStatus: status, errorClass: class, next: stop()}, err
	}
	if in.Stage == model.StageWork {
		return finishWork(ctx, tx, e, in)
	}
	var result contract.Result
	err := json.Unmarshal(in.Result, &result)
	if err == nil && result.Stage != in.Stage {
		err = fmt.Errorf("stage: differs from the reported stage")
	}
	profile, perr := readReportProfile(ctx, tx, e)
	if perr != nil {
		return finishOutcome{}, perr
	}
	declared := map[string]contract.ApprovalDeclaration{}
	for _, a := range profile.Approvals {
		declared[a.Operation] = a
	}
	if err == nil {
		for _, a := range result.ApprovalRequests {
			if _, ok := declared[a.Operation]; !ok {
				err = fmt.Errorf("approvalRequests.operation: %q is not declared", a.Operation)
				break
			}
		}
	}
	if err != nil {
		message := truncate("result JSON does not match the contract: "+err.Error(), 1000)
		ferr := failExecution(ctx, tx, e, model.ErrorContract, &message)
		return finishOutcome{runStatus: model.StageFailed, errorClass: model.ErrorContract, next: stop()}, ferr
	}
	return finishProgram(ctx, tx, e, in, result, profile, declared)
}

func truncate(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
}

func finishWork(ctx context.Context, tx *sql.Tx, e *reportExecution, in StageFinish) (finishOutcome, error) {
	if q := in.Work.Question; len(q) > 0 && string(q) != "null" {
		if !validQuestion(q) {
			message := "out/question.json does not match the contract"
			err := failExecution(ctx, tx, e, model.ErrorContract, &message)
			return finishOutcome{runStatus: model.StageFailed, errorClass: model.ErrorContract, next: stop()}, err
		}
		if err := e.move(ctx, tx, model.ExecutionBlocked, "", "question"); err != nil {
			return finishOutcome{}, err
		}
		_, err := openDecision(ctx, tx, e, model.DecisionQuestion, model.CauseQuestion, nil, nil, q)
		return finishOutcome{runStatus: model.StageQuestion, next: stop()}, err
	}
	if *in.ExitCode != 0 {
		err := failExecution(ctx, tx, e, model.ErrorExecutionFailed, nil)
		return finishOutcome{runStatus: model.StageFailed, errorClass: model.ErrorExecutionFailed, next: stop()}, err
	}
	return finishOutcome{runStatus: model.StageSucceeded, next: StageNext{Next: string(model.StageCheck)}}, nil
}

// openDecision creates an owner decision and its decision_opened event
// (spec 04 3.5). Notification follows the Execution transition.
func openDecision(ctx context.Context, tx *sql.Tx, e *reportExecution, kind model.DecisionKind, cause model.DecisionCause,
	subjectID, basis *string, detail any) (string, error) {
	var raw []byte
	if r, ok := detail.(json.RawMessage); ok {
		raw = r
	} else {
		var err error
		if raw, err = json.Marshal(detail); err != nil {
			return "", err
		}
	}
	id := ulid.Make().String()
	var causeValue any
	if cause != "" {
		causeValue = cause
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO owner_decisions (id, kind, task_id, execution_id, subject_id, cause, basis, detail, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'open', ?)`, id, kind, e.taskID, e.id, subjectID, causeValue, basis, string(raw),
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	return id, appendEvent(ctx, tx, e.id, model.EventDecisionOpened, map[string]any{"decisionId": id, "kind": kind, "cause": causeValue})
}

func finishProgram(ctx context.Context, tx *sql.Tx, e *reportExecution, in StageFinish, result contract.Result,
	profile reportProfile, declared map[string]contract.ApprovalDeclaration) (finishOutcome, error) {
	out := finishOutcome{runStatus: result.Status, next: stop()}
	if result.ErrorClass != nil {
		out.errorClass = *result.ErrorClass
	}
	if in.Stage == model.StageCheck {
		switch result.Status {
		case model.StageSucceeded, model.StageNeedsApproval, model.StageBlocked:
			if _, err := tx.ExecContext(ctx, `UPDATE executions SET content_id = ? WHERE id = ?`, *result.ContentID, e.id); err != nil {
				return out, err
			}
		}
		if err := recordCheckResult(ctx, tx, e, in, result); err != nil {
			return out, err
		}
	}
	switch {
	case result.Status == model.StageFailed:
		class := out.errorClass
		if in.Stage == model.StageCheck {
			class = model.ErrorTestFailed // spec 04 3.4.1: check failed is check_failed.
		}
		out.errorClass = class
		return out, failExecution(ctx, tx, e, class, &result.Message)
	case in.Stage == model.StagePrepare:
		for _, c := range result.ReportedContext {
			if _, err := tx.ExecContext(ctx, `INSERT INTO context_revisions (id, execution_id, layer, kind, source_path, source_revision, content_hash, content, created_at)
				VALUES (?, ?, 'reported', 'reported', ?, ?, ?, NULL, ?)`, ulid.Make().String(), e.id, c.Source+":"+c.Path, c.Revision, c.SHA256,
				time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return out, err
			}
		}
		out.next = StageNext{Next: string(model.StageWork)}
		return out, nil
	case in.Stage == model.StageCheck && result.Status == model.StageBlocked:
		if err := e.move(ctx, tx, model.ExecutionBlocked, "", "check_blocked"); err != nil {
			return out, err
		}
		detail := map[string]any{"message": result.Message, "summary": result.Summary, "findings": nonNil(result.Findings),
			"approvalRequests": nonNil(result.ApprovalRequests)}
		_, err := openDecision(ctx, tx, e, model.DecisionBlocked, model.CauseCheckBlocked, nil, result.ContentID, detail)
		return out, err
	case in.Stage == model.StagePublish && result.Status == model.StageUnknown:
		if err := e.move(ctx, tx, model.ExecutionBlocked, "", "publish_unknown"); err != nil {
			return out, err
		}
		detail := map[string]any{"message": result.Message, "summary": result.Summary}
		_, err := openDecision(ctx, tx, e, model.DecisionBlocked, model.CausePublishUnknown, nil, nil, detail)
		return out, err
	case in.Stage == model.StagePublish && result.Status == model.StageSucceeded && len(result.ApprovalRequests) == 0:
		return out, publishSucceeded(ctx, tx, e, result, profile)
	}
	// check succeeded / needs_approval, publish needs_approval (or succeeded
	// with approval requests, contract 6.5).
	waivers, err := routeWaivers(ctx, tx, e)
	if err != nil {
		return out, err
	}
	var waived []contract.RequestApproval
	var requested []contract.ApprovalRequest
	for _, a := range result.ApprovalRequests {
		if declared[a.Operation].Waivable && slices.Contains(waivers, a.Operation) {
			content := a.ContentID
			waived = append(waived, contract.RequestApproval{Operation: a.Operation, ContentID: &content, Status: "waived"})
			if err := appendEvent(ctx, tx, e.id, model.EventApprovalWaived, map[string]any{"operation": a.Operation, "contentId": a.ContentID}); err != nil {
				return out, err
			}
			continue
		}
		requested = append(requested, a)
	}
	if len(requested) > 0 {
		for _, a := range requested {
			if err := requestApproval(ctx, tx, e, in.Stage, a); err != nil {
				return out, err
			}
		}
		return out, e.move(ctx, tx, model.ExecutionWaitingApproval, "", "approval_required")
	}
	if in.Stage == model.StagePublish {
		// Every operation was waived: start the publish stage again (spec 17 3.3 step 8).
		return out, e.move(ctx, tx, model.ExecutionPending, model.StagePublish, "approval_waived")
	}
	out.next = StageNext{Next: string(model.StagePublish), CheckedContentID: result.ContentID, Approvals: waived}
	return out, nil
}

func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

func recordCheckResult(ctx context.Context, tx *sql.Tx, e *reportExecution, in StageFinish, result contract.Result) error {
	value := map[model.StageStatus]string{
		model.StageSucceeded: "pass", model.StageNeedsApproval: "pass", model.StageFailed: "fail", model.StageBlocked: "blocked",
	}[result.Status]
	findings, err := json.Marshal(nonNil(result.Findings))
	if err != nil {
		return err
	}
	var runID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM stage_runs WHERE execution_id = ? AND stage = ? AND attempt = ?`,
		e.id, in.Stage, in.Attempt).Scan(&runID); err != nil {
		return err
	}
	id := ulid.Make().String()
	if _, err := tx.ExecContext(ctx, `INSERT INTO check_results (id, execution_id, stage_run_id, result, content_id, summary, findings, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, e.id, runID, value, result.ContentID, result.Summary, string(findings),
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return appendEvent(ctx, tx, e.id, model.EventCheckResult, map[string]any{
		"checkResultId": id, "result": value, "findingCount": len(result.Findings), "contentId": result.ContentID,
	})
}

func routeWaivers(ctx context.Context, tx *sql.Tx, e *reportExecution) ([]string, error) {
	var raw string
	var err error
	switch e.routeKind.String {
	case string(model.RouteProject):
		err = tx.QueryRowContext(ctx, `SELECT approval_waivers FROM projects WHERE id = ?`, e.projectID.String).Scan(&raw)
	case string(model.RouteHarness):
		err = tx.QueryRowContext(ctx, `SELECT approval_waivers FROM harnesses WHERE id = ?`, e.harnessID.String).Scan(&raw)
	default:
		return nil, fmt.Errorf("store: Execution %s has no pinned route", e.id)
	}
	if err != nil {
		return nil, err
	}
	var waivers []string
	if err := json.Unmarshal([]byte(raw), &waivers); err != nil {
		return nil, fmt.Errorf("store: approval_waivers cannot be read")
	}
	return waivers, nil
}

// requestApproval creates the pending approval and its owner decision for
// one operation, superseding the unused approval of the same operation
// (spec 17 3.3 steps 3-5, 3.4).
func requestApproval(ctx context.Context, tx *sql.Tx, e *reportExecution, stage model.Stage, a contract.ApprovalRequest) error {
	if err := invalidateApprovalsWhereTx(ctx, tx, "superseded", `execution_id = ? AND operation = ?`, e.id, a.Operation); err != nil {
		return err
	}
	var target any
	if err := json.Unmarshal(a.Target, &target); err != nil {
		return err
	}
	normalized, err := json.Marshal(map[string]any{"operation": a.Operation, "target": target, "contentId": a.ContentID})
	if err != nil {
		return err
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(normalized)
	approvalID := ulid.Make().String()
	content := a.ContentID
	decisionID, err := openDecision(ctx, tx, e, model.DecisionApproval, "", &approvalID, &content, map[string]any{
		"operation": a.Operation, "target": target, "summary": a.Summary, "contentId": a.ContentID, "requestedStage": stage,
	})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO approvals (id, task_id, execution_id, decision_id, requested_stage, operation, target,
		summary, content_id, params_hash, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		approvalID, e.taskID, e.id, decisionID, stage, a.Operation, string(targetJSON), a.Summary, a.ContentID,
		hex.EncodeToString(sum[:]), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return appendEvent(ctx, tx, e.id, model.EventApprovalRequested, map[string]any{
		"approvalId": approvalID, "operation": a.Operation, "contentId": a.ContentID,
	})
}

// publishSucceeded writes outputs and the records, then completes the
// Execution (spec 04 3.4.1, contract 9).
func publishSucceeded(ctx context.Context, tx *sql.Tx, e *reportExecution, result contract.Result, profile reportProfile) error {
	outputs, err := json.Marshal(nonNil(result.Outputs))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE executions SET outputs = ? WHERE id = ?`, string(outputs), e.id); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, e.id, model.EventPublished, map[string]any{"outputs": json.RawMessage(outputs)}); err != nil {
		return err
	}
	if profile.Records != nil && len(profile.Records.Keep) > 0 {
		if err := keepRecords(ctx, tx, e, profile.Records.Keep); err != nil {
			return err
		}
	}
	return e.move(ctx, tx, model.ExecutionSucceeded, "", "stage_finished")
}

// keepRecords registers the latest out_file of each records.keep entry
// ("<stage>/<path in out>") as a harness record. A declared file the stage
// did not write is skipped.
func keepRecords(ctx context.Context, tx *sql.Tx, e *reportExecution, keep []string) error {
	var identifier sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT l.issue_identifier FROM tasks t LEFT JOIN linear_issue_links l ON l.task_id = t.id
		WHERE t.id = ?`, e.taskID).Scan(&identifier); err != nil {
		return err
	}
	now := time.Now().UTC()
	source := e.taskID
	if identifier.Valid && identifier.String != "" {
		source = identifier.String
	}
	for _, entry := range keep {
		stage, name, ok := cutStage(entry)
		if !ok {
			continue
		}
		var artifactID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM artifacts WHERE execution_id = ? AND kind = 'out_file' AND stage = ? AND name = ?
			ORDER BY created_at DESC, id DESC LIMIT 1`, e.id, stage, name).Scan(&artifactID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		fileName := now.Format("2006-01-02") + "-" + source + "-" + path.Base(name)
		if _, err := tx.ExecContext(ctx, `INSERT INTO harness_records (id, harness_id, artifact_id, execution_id, task_id, source_identifier, file_name, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, ulid.Make().String(), e.harnessID, artifactID, e.id, e.taskID, identifier, fileName,
			now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}

func cutStage(entry string) (string, string, bool) {
	stage, name, ok := strings.Cut(entry, "/")
	return stage, name, ok && stage != "" && name != ""
}
