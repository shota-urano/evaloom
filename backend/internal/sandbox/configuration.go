package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"strings"
	"sync"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
)

type configurationKey struct{}

// WithWorkingConfiguration is an internal capability, not a SandboxSpec/API
// field. It binds verified bytes to an execution-owned Docker volume. A host
// path is never used as a writable configuration mount: Docker resolves bind
// sources later, potentially after the verified directory has been replaced.
// Close must be called after every Process has confirmed termination.
func WithWorkingConfiguration(ctx context.Context, env *harnessenv.Environment) (context.Context, func() error, error) {
	files, err := env.Files(ctx)
	if err != nil {
		return ctx, nil, err
	}
	authName, authData, err := env.AuthenticationFile()
	if err != nil {
		return ctx, nil, &harnesscontext.ConfigError{Err: err}
	}
	var data bytes.Buffer
	tw := tar.NewWriter(&data)
	dirs := map[string]bool{}
	expected := map[string]string{}
	for _, file := range files {
		expected[file.Path] = file.ContentHash
		var parents []string
		for dir := path.Dir(file.Path); dir != "."; dir = path.Dir(dir) {
			parents = append(parents, dir)
		}
		for i := len(parents) - 1; i >= 0; i-- {
			dir := parents[i]
			if !dirs[dir] {
				if err := tw.WriteHeader(&tar.Header{Name: dir + "/", Mode: 0700, Typeflag: tar.TypeDir}); err != nil {
					return ctx, nil, err
				}
				dirs[dir] = true
			}
		}
		if err := tw.WriteHeader(&tar.Header{Name: file.Path, Mode: 0600, Size: int64(len(file.Content)), Typeflag: tar.TypeReg}); err != nil {
			return ctx, nil, err
		}
		if _, err := tw.Write([]byte(file.Content)); err != nil {
			return ctx, nil, err
		}
	}
	if authName != "" {
		if err := tw.WriteHeader(&tar.Header{Name: authName, Mode: 0600, Size: int64(len(authData)), Typeflag: tar.TypeReg}); err != nil {
			return ctx, nil, err
		}
		if _, err := tw.Write(authData); err != nil {
			return ctx, nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return ctx, nil, err
	}
	w := &workingConfiguration{env: env, authName: authName, archive: data.Bytes(), expected: expected, volume: "evaloom-config-" + strings.ToLower(ulid.Make().String())}
	return context.WithValue(ctx, configurationKey{}, w), w.close, nil
}

type workingConfiguration struct {
	mu             sync.Mutex
	env            *harnessenv.Environment
	authName       string
	archive        []byte
	expected       map[string]string
	volume         string
	owner          *Container
	seeded, closed bool
}

func (w *workingConfiguration) verify(ctx context.Context, spec SandboxSpec) error {
	if w.closed || w.env.ConfigDir() != spec.ConfigDir || spec.Kind != Harness || !spec.ConfigWritable {
		return &harnesscontext.ConfigError{Err: fmt.Errorf("configuration capability mismatch")}
	}
	_, err := w.env.Files(ctx)
	return err
}

func (w *workingConfiguration) prepare(ctx context.Context, c *Container) error {
	if w.owner != nil {
		if w.owner.config.RuntimePath != c.config.RuntimePath || !maps.Equal(w.owner.config.RuntimeEnv, c.config.RuntimeEnv) {
			return &Error{Op: "configuration runtime mismatch"}
		}
		return nil
	}
	// Retain ownership before creation, including a lost acknowledgement.
	w.owner = c
	if _, err := c.output(ctx, "volume", "create", "--driver", "local", w.volume); err != nil {
		return &Error{Op: "configuration volume creation failed", Cause: ctx.Err()}
	}
	return nil
}

func (w *workingConfiguration) seed(ctx context.Context, c *Container, name string) error {
	if w.seeded {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.config.ControlTimeout)
	defer cancel()
	// cp reads this immutable tar from stdin, never reopens a host config path.
	cmd := c.command(ctx, "container", "cp", "-", name+":"+w.env.ConfigDir()+"/.")
	cmd.Stdin = bytes.NewReader(w.archive)
	if err := cmd.Run(); err != nil {
		return &Error{Op: "configuration transfer failed", Cause: ctx.Err()}
	}
	// Verify the actual mounted volume, not only its staging input. The daemon
	// retains this volume identity across both commands; no host path is later
	// resolved as its source. cp does not start any container workload.
	actual, err := c.output(ctx, "container", "cp", name+":"+w.env.ConfigDir()+"/.", "-")
	if err != nil {
		return &Error{Op: "configuration readback failed", Cause: ctx.Err()}
	}
	if err := w.verifyArchive(actual); err != nil {
		return err
	}
	w.seeded = true
	return nil
}

func (w *workingConfiguration) verifyArchive(data []byte) error {
	tr := tar.NewReader(bytes.NewReader(data))
	seen := map[string]bool{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &harnesscontext.ConfigError{Err: fmt.Errorf("invalid configuration archive")}
		}
		name := strings.TrimSuffix(strings.TrimPrefix(header.Name, "./"), "/")
		if name == "" {
			name = "."
		}
		if !fs.ValidPath(name) || strings.ContainsAny(name, "\x00\\") {
			return &harnesscontext.ConfigError{Err: fmt.Errorf("invalid configuration archive path")}
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if name == w.authName && w.authName != "" {
			if header.Typeflag != tar.TypeReg || seen[name] || header.Size > harnessenv.AuthenticationLimit {
				return &harnesscontext.ConfigError{Err: fmt.Errorf("invalid mounted authentication file")}
			}
			data, err := io.ReadAll(io.LimitReader(tr, harnessenv.AuthenticationLimit+1))
			if err != nil || harnessenv.ValidateAuthentication(data) != nil {
				return &harnesscontext.ConfigError{Err: fmt.Errorf("invalid mounted authentication file")}
			}
			seen[name] = true
			continue
		}
		want, ok := w.expected[name]
		if header.Typeflag != tar.TypeReg || !ok || seen[name] {
			return &harnesscontext.ConfigError{Err: fmt.Errorf("unselected or non-regular configuration file")}
		}
		h := sha256.New()
		if _, err := io.Copy(h, tr); err != nil || fmt.Sprintf("%x", h.Sum(nil)) != want {
			return &harnesscontext.ConfigError{Err: fmt.Errorf("mounted configuration differs from pinned file")}
		}
		seen[name] = true
	}
	count := len(w.expected)
	if w.authName != "" {
		count++
	}
	if len(seen) != count {
		return &harnesscontext.ConfigError{Err: fmt.Errorf("mounted configuration file missing")}
	}
	return nil
}

func (w *workingConfiguration) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	w.archive = nil
	if w.owner == nil {
		return nil
	}
	c := w.owner
	_, err := c.output(context.Background(), "volume", "rm", "--force", w.volume)
	if err == nil {
		return nil
	}
	// Like container removal, confirm absence after a lost acknowledgement.
	out, checkErr := c.output(context.Background(), "volume", "ls", "--filter", "name=^"+w.volume+"$", "--format", "{{.Name}}")
	if checkErr == nil && strings.TrimSpace(string(out)) == "" {
		return nil
	}
	return &Error{Op: "configuration volume removal unconfirmed", Cause: errors.Join(err, checkErr)}
}
