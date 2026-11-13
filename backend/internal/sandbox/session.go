package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

// Called only after stopped writers and credential writeback, before close
// removes the volume. The recovery container is read-only and never started.
func (w *workingConfiguration) captureSession() error {
	if w.owner == nil {
		return w.env.CaptureSession(context.Background())
	}
	if !w.seeded {
		return nil
	}
	c := w.owner
	ctx, cancel := context.WithTimeout(context.Background(), c.config.ControlTimeout)
	defer cancel()
	name := "evaloom-session-" + strings.ToLower(ulid.Make().String())
	defer c.remove(name)
	if _, err := c.output(ctx, "container", "create", "--name", name, "--network", "none", "--read-only",
		"--mount", "type=volume,source="+w.volume+",target=/evaloom-session,readonly,volume-nocopy", c.config.Image, "/bin/true"); err != nil {
		return fmt.Errorf("session recovery container creation failed")
	}
	cmd := c.command(ctx, "container", "cp", name+":/evaloom-session/.", "-")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	readErr := w.env.CaptureSessionArchive(ctx, pipe)
	if readErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil || waitErr != nil {
		w.env.DiscardSession()
	}
	return errors.Join(readErr, waitErr)
}
