package notification

import (
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// notificationContent assembles the transport-independent request body from
// spec 12 3.1 metadata. Each transport may apply a stricter content policy.
func notificationContent(n Notification) (string, string, error) {
	title, body, err := classifiedContent(n)
	if err != nil || n.DecisionKind != "" {
		return title, body, err
	}
	if n.Status == model.ExecutionSucceeded && n.OutputLabel != "" {
		body += "\n" + n.OutputLabel
		if n.OutputRemaining > 0 {
			body += fmt.Sprintf(" ほか%d件", n.OutputRemaining)
		}
	}
	if n.Status == model.ExecutionFailed || n.Status == model.ExecutionInterrupted || n.Status == model.ExecutionTimedOut {
		stage := map[model.ExecutionStatus]string{
			model.ExecutionPreparing: "prepare", model.ExecutionRunning: "work",
			model.ExecutionVerifying: "check", model.ExecutionPublishing: "publish",
		}[n.Stage]
		if stage != "" {
			body += "\n" + stage
		}
	}
	return title, body, nil
}

// classifiedContent allows only title, ID and validated classifications. Never
// use free-form request bodies, output metadata or stage names here: this is
// also the desktop notification's privacy boundary (Q17).
func classifiedContent(n Notification) (string, string, error) {
	if n.DecisionKind != "" {
		kind := map[model.DecisionKind]string{
			model.DecisionRoute: "振り分け", model.DecisionApproval: "承認",
			model.DecisionBlocked: "続行", model.DecisionQuestion: "質問",
		}[n.DecisionKind]
		if kind == "" || n.DecisionID == "" {
			return "", "", errors.New("notification: invalid decision notification")
		}
		body := n.Title + "\n" + n.TaskID + "\n" + kind
		if n.DecisionKind == model.DecisionBlocked {
			cause := map[model.DecisionCause]string{
				model.CauseCheckBlocked:     "確かめた結果に本人の判断が必要",
				model.CausePublishUnknown:   "出す操作の結果がわからない",
				model.CauseContextConflict:  "固定した文脈に解決できない矛盾",
				model.CauseWorkspaceMissing: "再開に要る作業領域がない",
			}[n.DecisionCause]
			if cause == "" {
				return "", "", errors.New("notification: invalid decision cause")
			}
			body += "\n" + cause
		}
		return "evaloom: 判断待ち", body, nil
	}
	var title string
	switch n.Status {
	case model.ExecutionSucceeded:
		title = "evaloom: 完了"
	case model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut:
		title = "evaloom: 失敗"
	case model.ExecutionBlocked:
		title = "evaloom: 対応が必要"
	case model.ExecutionWaitingApproval:
		title = "evaloom: 承認待ち"
	default:
		return "", "", errors.New("notification: unsupported macOS notification status")
	}
	classification := string(n.Status)
	if n.Status != model.ExecutionSucceeded && n.Status != model.ExecutionWaitingApproval && n.ErrorClass != "" && n.ErrorClass.Validate() == nil {
		classification = string(n.ErrorClass)
	}
	body := n.Title + "\n" + n.TaskID + "\n" + classification
	return title, body, nil
}
