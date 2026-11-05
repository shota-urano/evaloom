package sandbox

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
)

// WriteBackAuthentication is called only after every launched Process.Wait has
// confirmed termination. Recovery must survive the execution's cancellation.
// Diagnostic output from the runtime is never forwarded to execution logs.
func WriteBackAuthentication(ctx context.Context) error {
	w, _ := ctx.Value(configurationKey{}).(*workingConfiguration)
	if w == nil || w.authName == "" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("authentication: configuration already closed")
	}
	if w.owner == nil {
		return w.env.WriteBackAuthentication(nil)
	}
	if !w.seeded {
		return nil
	} // no CLI could start on an unverified volume
	c := w.owner
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.ControlTimeout)
	defer cancel()
	name := "evaloom-auth-" + strings.ToLower(ulid.Make().String())
	// Mount the existing private volume read-only in a container that is NEVER
	// started. cp archives the target inode; it does not follow symlinks (-L).
	defer c.remove(name)
	if _, err := c.output(ctx, "container", "create", "--name", name, "--network", "none", "--read-only",
		"--mount", "type=volume,source="+w.volume+",target=/evaloom-auth,readonly,volume-nocopy", c.config.Image, "/bin/true"); err != nil {
		return fmt.Errorf("authentication: recovery container creation failed")
	}
	cmd := c.command(ctx, "container", "cp", name+":/evaloom-auth/"+w.authName, "-")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("authentication: recovery pipe failed")
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("authentication: recovery command failed")
	}
	// Bound both tar metadata and payload before the untrusted archive is parsed.
	// 64 KiB of metadata covers Docker's headers; no payload above 64 KiB is used.
	data, readErr := authenticationArchive(io.LimitReader(pipe, 2*harnessenv.AuthenticationLimit), w.authName)
	if readErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return fmt.Errorf("authentication: recovery command failed")
	}
	return w.env.WriteBackAuthentication(data)
}

func authenticationArchive(source io.Reader, name string) ([]byte, error) {
	tr := tar.NewReader(source)
	h, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("authentication: recovery archive invalid")
	}
	if h.Name != name || h.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("authentication: recovered file is not regular or has unexpected path")
	}
	if h.Size > harnessenv.AuthenticationLimit {
		return nil, fmt.Errorf("authentication: file exceeds 64 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(tr, harnessenv.AuthenticationLimit+1))
	if err != nil {
		return nil, fmt.Errorf("authentication: recovery read failed")
	}
	if err := harnessenv.ValidateAuthentication(data); err != nil {
		return nil, err
	}
	if _, err := tr.Next(); err != io.EOF {
		return nil, fmt.Errorf("authentication: recovery archive contains extra entries")
	}
	// Drain the bounded stream so Wait cannot block on an unread tar trailer.
	if _, err := io.Copy(io.Discard, source); err != nil {
		return nil, fmt.Errorf("authentication: recovery read failed")
	}
	return data, nil
}
