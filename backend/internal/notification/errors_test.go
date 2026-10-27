package notification

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestFailureReasonUsesErrorIdentity(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{ErrPermissionDenied, "permission_denied"},
		{ErrLaunchFailed, "launch_failed"},
		{ErrTimeout, "timeout"},
		{context.DeadlineExceeded, "timeout"},
		{context.Canceled, "other"},
		{errors.New("permission_denied launch_failed timeout"), "other"},
		{errors.New(ErrPermissionDenied.Error()), "other"},
		{errors.New(ErrLaunchFailed.Error()), "other"},
		{errors.New(ErrTimeout.Error()), "other"},
	} {
		for _, err := range []error{tc.err, fmt.Errorf("private transport detail: %w", tc.err)} {
			if got := failureReason(err); got != tc.want {
				t.Errorf("failureReason(%v) = %q; want %q", err, got, tc.want)
			}
		}
	}
}
