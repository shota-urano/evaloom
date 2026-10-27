package notification

import (
	"context"
	"errors"
)

// Transports wrap these sentinels to classify failures without inspecting
// error text. Additional details remain available to the Control Plane log.
var (
	ErrPermissionDenied = errors.New("notification: permission denied")
	ErrLaunchFailed     = errors.New("notification: helper launch failed")
	ErrTimeout          = errors.New("notification: helper result timed out")
)

func failureReason(err error) string {
	switch {
	case errors.Is(err, ErrPermissionDenied):
		return "permission_denied"
	case errors.Is(err, ErrLaunchFailed):
		return "launch_failed"
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "other"
	}
}
