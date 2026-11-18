package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// Launch is the Work Sandbox entry point Runner hands to work Adapters
// (runner.WorkAdapterConfig.Launch). Every command runs in a new container
// built from the same stage specification; nothing runs on the host.
type Launch func(context.Context, adapter.SandboxEnv, []string, io.Writer, io.Writer) (sandbox.Process, error)

// LaunchSandbox adapts Launch to Sandbox, so the probe Adapter runs inside
// the real Work Sandbox wired by Runner.
type LaunchSandbox struct {
	Launch Launch
}

// Check cannot prove availability without a session's environment, so it
// always fails; the Adapter calls CheckEnv instead.
func (s LaunchSandbox) Check(context.Context) error {
	if s.Launch == nil {
		return ErrUnavailable
	}
	return errors.New("probe: LaunchSandbox is checked per session (CheckEnv)")
}

// CheckEnv proves availability by running a command in the Sandbox with the
// session's environment, not by the mere presence of configuration.
func (s LaunchSandbox) CheckEnv(ctx context.Context, env adapter.SandboxEnv) error {
	out, err := s.Run(ctx, env, []string{"/usr/bin/env", "true"})
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("probe: sandbox check exited %d", out.ExitCode)
	}
	return nil
}

func (s LaunchSandbox) Run(ctx context.Context, env adapter.SandboxEnv, command []string) (Output, error) {
	if s.Launch == nil {
		return Output{}, ErrUnavailable
	}
	var stdout, stderr bytes.Buffer
	process, err := s.Launch(ctx, env, command, &stdout, &stderr)
	if err != nil {
		return Output{}, err
	}
	err = process.Wait()
	code, known := process.ExitCode()
	var exit *sandbox.ExitError
	if !known || err != nil && !errors.As(err, &exit) {
		if err == nil {
			err = errors.New("probe: exit code unknown")
		}
		return Output{}, err
	}
	return Output{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// ReadFile reads a worktree-relative file inside the same Sandbox. The
// working directory of a work command is the worktree.
func (s LaunchSandbox) ReadFile(ctx context.Context, env adapter.SandboxEnv, name string) ([]byte, error) {
	if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "..") {
		return nil, fmt.Errorf("probe: worktree-relative report path required")
	}
	out, err := s.Run(ctx, env, []string{"/bin/cat", name})
	if err != nil {
		return nil, err
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("probe: cannot read %s", name)
	}
	return []byte(out.Stdout), nil
}
