package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The CLI is only a client of the runtime. Killing it alone does not stop a
// container. Process groups here bound stuck clients/helpers, not the sandbox.
func (c *Container) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.config.RuntimePath, args...)
	cmd.Env = append([]string{}, c.env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = c.config.ControlTimeout
	return cmd
}

func (c *Container) output(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.ControlTimeout)
	defer cancel()
	return c.command(ctx, args...).Output()
}

func (c *Container) remove(name string) error {
	_, err := c.output(context.Background(), "container", "rm", "--force", "--volumes", name)
	if err == nil {
		return nil // Successful forced removal confirms all container tasks stopped.
	}
	// rm is not idempotent on Docker. A successful empty listing confirms that
	// another concurrent Kill/removal has already removed precisely this name.
	out, listErr := c.output(context.Background(), "container", "ls", "--all", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
	if listErr == nil && strings.TrimSpace(string(out)) == "" {
		return nil
	}
	return &Error{Op: "container removal unconfirmed"}
}

type containerProcess struct {
	container   *Container
	name        string
	done        chan struct{}
	removed     chan struct{}
	removeOnce  sync.Once
	err         error // Published by closing done.
	exitCode    int
	exitCodeSet bool
}

func (p *containerProcess) Wait() error {
	<-p.done
	return p.err
}

func (p *containerProcess) ExitCode() (int, bool) {
	select {
	case <-p.done:
		return p.exitCode, p.exitCodeSet
	default:
		return 0, false
	}
}

func (p *containerProcess) finished() bool {
	select {
	case <-p.done:
		return true
	case <-p.removed:
		return true
	default:
		return false
	}
}

func (p *containerProcess) Stop() error {
	if p.finished() {
		return nil
	}
	_, err := p.container.output(context.Background(), "container", "kill", "--signal", "TERM", p.name)
	if err != nil && !p.finished() {
		return &Error{Op: "container stop request failed"}
	}
	return nil
}

func (p *containerProcess) Kill() error {
	if p.finished() {
		return nil
	}
	if err := p.container.remove(p.name); err != nil {
		return err
	}
	p.removeOnce.Do(func() { close(p.removed) })
	return nil
}

type containerState struct {
	Status    string
	Running   bool
	OOMKilled bool
	ExitCode  int
	Error     string
}

func (p *containerProcess) state(ctx context.Context) (containerState, error) {
	out, err := p.container.output(ctx, "container", "inspect", "--format", "{{json .State}}", p.name)
	var state containerState
	if err != nil || json.Unmarshal(out, &state) != nil {
		return state, &Error{Op: "container state unconfirmed"}
	}
	return state, nil
}

func (s containerState) stopped() bool {
	// A still-pending start request may transition created -> running after
	// inspection. Only terminal states or confirmed removal are sufficient.
	return !s.Running && (s.Status == "exited" || s.Status == "dead")
}

func (p *containerProcess) supervise(ctx context.Context, cancel context.CancelFunc, attach *exec.Cmd, stopAttach context.CancelFunc) {
	defer close(p.done)
	defer cancel()
	defer stopAttach()
	cliDone := make(chan error, 1)
	go func() { cliDone <- attach.Wait() }()
	var cliErr error
	var interrupted bool
	select {
	case cliErr = <-cliDone:
	case <-ctx.Done():
		interrupted = true
	case <-p.removed:
		interrupted = true
	}
	if !interrupted {
		// The attach client may exit while the workload is still running.
		// Inspection must yield at the execution deadline so it cannot delay
		// the first forced removal until the control-command timeout.
		state, err := p.state(ctx)
		switch {
		case err != nil || !state.stopped():
			p.err = &Error{Op: "container execution state unconfirmed"}
		default:
			p.exitCode, p.exitCodeSet = state.ExitCode, true
			switch {
			case state.OOMKilled || state.Error != "" || state.ExitCode >= 128:
				p.err = &Error{Op: "container resource limit or runtime failure"}
			case state.ExitCode != 0:
				p.err = &ExitError{Code: state.ExitCode}
			case cliErr != nil:
				p.err = &Error{Op: "container attach failed"}
			}
		}
	}
	// Even an exited CLI may leave the container running. Never release Runner
	// capacity based only on the client's exit. Removal, or inspected stopped
	// state, is the confirmation boundary. If the daemon cannot be reached and
	// a running container cannot be ruled out, Wait deliberately stays pending.
	for {
		if err := p.Kill(); err == nil {
			break
		}
		// Cleanup needs a fresh control deadline after the execution expires:
		// Wait must still confirm removal or a terminal container state.
		if state, err := p.state(context.Background()); err == nil && state.stopped() {
			p.err = &Error{Op: "stopped container cleanup failed"}
			break
		}
		select {
		case <-p.removed:
		case <-time.After(p.container.config.ControlTimeout):
		}
	}
	// Only after stopping the container do we terminate a stuck attach client.
	if interrupted {
		stopAttach()
		<-cliDone
		p.err = &Error{Op: "container interrupted", Cause: ctx.Err()}
	} else if ctx.Err() != nil {
		p.err = &Error{Op: "container interrupted", Cause: ctx.Err()}
	}
}
