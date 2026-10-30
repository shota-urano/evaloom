// Package publisher pushes verified commits and creates pull requests for the
// Trusted Runner (spec 11). It never merges and never force-pushes.
package publisher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

// BranchPrefix is the only namespace Git Publisher may push to (spec 11, 3.1).
const BranchPrefix = "evaloom/"

var objectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Errors returned by CheckBeforePush, other than context cancellation, are
// publish_failed (spec 11, section 6). Their text never includes Git output,
// branch names or Control Plane responses.
var (
	ErrControlPlaneUnavailable = errors.New("publisher: cancellation check failed")
	ErrBranchName              = errors.New("publisher: branch is outside the evaloom/ namespace")
	ErrVerifiedSHA             = errors.New("publisher: verified SHA is not a full object ID")
	ErrHeadMismatch            = errors.New("publisher: HEAD is not the verified commit")
	ErrDirtyWorktree           = errors.New("publisher: worktree has uncommitted changes")
	ErrGit                     = errors.New("publisher: git inspection failed")
)

// ErrorClass maps a CheckBeforePush error to the Execution error class.
// Cancellation is reported by the executor as cancelled.
func ErrorClass(err error) model.ErrorClass {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return model.ErrorCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return model.ErrorTimeout
	default:
		return model.ErrorPublishFailed
	}
}

// Target is the commit and branch one push would publish.
type Target struct {
	// Worktree is the task worktree inside the Runner-owned clone.
	Worktree    string
	VerifiedSHA string
	BranchName  string
}

// CheckBeforePush runs the spec 11, 3.1 checks and must be called immediately
// before git push, after sandbox writers have stopped. The local Git checks run
// first so that the cancellation check, which needs the Control Plane, is the
// last step before the push. checkCancellation is typically Runner.Heartbeat:
// it cancels ctx when the Control Plane requests cancellation. Any failure,
// including an unreachable Control Plane, means the caller must not push.
func CheckBeforePush(ctx context.Context, target Target, checkCancellation func(context.Context) error) error {
	if checkCancellation == nil {
		return fmt.Errorf("publisher: cancellation check is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkBranchName(ctx, target); err != nil {
		return err
	}
	if err := checkWorktree(ctx, target); err != nil {
		return err
	}
	checkErr := checkCancellation(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if checkErr != nil {
		// The check's own error may echo URLs or responses; keep only the cause.
		return ErrControlPlaneUnavailable
	}
	return nil
}

func checkBranchName(ctx context.Context, target Target) error {
	name := target.BranchName
	if !strings.HasPrefix(name, BranchPrefix) || len(name) == len(BranchPrefix) {
		return ErrBranchName
	}
	// Reject names Git would not store verbatim (for example "..", "@{" or a
	// trailing ".lock"), so the pushed ref is exactly the checked name.
	if _, err := safegit.Output(ctx, target.Worktree, "check-ref-format", "refs/heads/"+name); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return ErrBranchName
	}
	return nil
}

func checkWorktree(ctx context.Context, target Target) error {
	if !objectID.MatchString(target.VerifiedSHA) {
		return ErrVerifiedSHA
	}
	head, err := gitOutput(ctx, target.Worktree, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return err
	}
	if string(bytes.TrimSpace(head)) != target.VerifiedSHA {
		return ErrHeadMismatch
	}
	// Untracked files count as changes: the tested tree may have included them.
	// Ignored files do not, matching git status.
	status, err := gitOutput(ctx, target.Worktree, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return ErrDirtyWorktree
	}
	return nil
}

func gitOutput(ctx context.Context, worktree string, args ...string) ([]byte, error) {
	out, err := safegit.Output(ctx, worktree, args...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, ErrGit
	}
	return out, nil
}
