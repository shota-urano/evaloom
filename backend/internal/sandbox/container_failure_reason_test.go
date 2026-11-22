package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// evaloom-6pi: the runtime's own reason (exit status and stderr) stays
// reachable through errors.As, while Error() still never contains it.
func TestContainerFailureKeepsRuntimeReason(t *testing.T) {
	for _, tc := range []struct {
		mode, op string
		stderr   bool
		detail   string
	}{
		{mode: "create-fail", op: "container creation failed", stderr: true},
		{mode: "image-fail", op: "Linux image without implicit volumes required", stderr: true},
		{mode: "image-darwin", op: "Linux image without implicit volumes required", detail: `image os "darwin"`},
		{mode: "inspect-fail", op: "container execution state unconfirmed", stderr: true},
		{mode: "oom", op: "container resource limit or runtime failure", detail: "oom killed true"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newFixture(t, tc.mode)
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"true"})
			if err == nil {
				err = p.Wait()
			}
			requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
			if !strings.Contains(err.Error(), tc.op) {
				t.Fatalf("error=%q; want op %q", err, tc.op)
			}
			var runtime *RuntimeError
			if !errors.As(err, &runtime) {
				t.Fatalf("runtime reason is not reachable: %#v", err)
			}
			if tc.stderr && (runtime.ExitCode != 125 || !strings.Contains(runtime.Stderr, "runtime diagnostic with SECRET_DO_NOT_REPORT")) {
				t.Fatalf("runtime=%+v; want exit 125 and the CLI stderr", runtime)
			}
			if tc.detail != "" && !strings.Contains(runtime.Detail, tc.detail) {
				t.Fatalf("runtime detail=%q; want %q", runtime.Detail, tc.detail)
			}
			if strings.Contains(runtime.Error(), "SECRET_DO_NOT_REPORT") {
				t.Fatal("RuntimeError.Error exposes stderr")
			}
		})
	}
}
