package notification

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

const resultEnvironment = "EVALOOM_NOTIFICATION_RESULT"

// MacOS launches the signed helper through LaunchServices, including when the
// Control Plane runs as a launchd user agent. It is portable for unit testing;
// the Control Plane only registers it on macOS.
type MacOS struct {
	helperPath string
	port       int
	run        func(*exec.Cmd) error
	timeout    time.Duration
}

func NewMacOS(helperPath string, port int) *MacOS {
	return &MacOS{helperPath: helperPath, port: port, run: (*exec.Cmd).Run, timeout: 30 * time.Second}
}

func (m *MacOS) Notify(ctx context.Context, n Notification) error {
	title, body, err := macOSContent(n)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(m.helperPath) || m.port < 1 || m.port > 65535 || n.TaskID == "" {
		return fmt.Errorf("notification: invalid macOS helper path, port or task ID")
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	// open returns before the helper asks for authorization. A private per-call
	// result file acknowledges authorization AND notification registration. The
	// open(1) environment is inherited by the newly launched app (-n).
	dir, err := os.MkdirTemp("", "evaloom-notification-")
	if err != nil {
		return fmt.Errorf("notification: prepare helper result: %w", err)
	}
	defer os.RemoveAll(dir)
	resultPath := filepath.Join(dir, "result")
	taskURL := "http://127.0.0.1:" + strconv.Itoa(m.port) + "/tasks/" + url.PathEscape(n.TaskID)
	cmd := exec.CommandContext(ctx, "/usr/bin/open", "-n", "-a", m.helperPath, "--args", title, body, taskURL)
	cmd.Env = append(os.Environ(), resultEnvironment+"="+resultPath)
	if err := m.run(cmd); err != nil {
		return fmt.Errorf("notification: launch macOS helper: %w", err)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := os.ReadFile(resultPath)
		if err == nil {
			switch string(result) {
			case "sent":
				return nil
			case "permission_denied":
				return errors.New("notification: macOS notification permission denied")
			case "authorization_failed", "registration_failed", "invalid_arguments", "timed_out":
				return fmt.Errorf("notification: macOS helper: %s", result)
			default:
				// Never copy arbitrary helper output into Control Plane logs.
				return errors.New("notification: invalid macOS helper result")
			}
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("notification: read macOS helper result: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("notification: wait for macOS helper: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Construct visible content from an allowlist. Free-form reasons, PR URLs,
// execution IDs and stage descriptions must never reach desktop notifications.
func macOSContent(n Notification) (string, string, error) {
	var title string
	switch n.Status {
	case model.ExecutionSucceeded:
		title = "evaloom: 完了"
	case model.ExecutionFailed, model.ExecutionInterrupted, model.ExecutionTimedOut:
		title = "evaloom: 失敗"
	case model.ExecutionBlocked:
		title = "evaloom: 対応が必要"
	default:
		return "", "", errors.New("notification: unsupported macOS notification status")
	}
	classification := string(n.Status)
	if n.Status == model.ExecutionBlocked && n.BlockCause != "" && n.BlockCause.Validate() == nil {
		classification = string(n.BlockCause)
	} else if n.Status != model.ExecutionSucceeded && n.ErrorClass != "" && n.ErrorClass.Validate() == nil {
		classification = string(n.ErrorClass)
	}
	return title, n.Title + "\n" + n.TaskID + "\n" + classification, nil
}
