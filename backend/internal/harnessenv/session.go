package harnessenv

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// Session carries a stopped CLI's configuration between question attempts.
// The Runner owns its lifetime. Authentication never enters this snapshot.
// Access is sequential: restore, run/stop, capture, then retain or discard.
type Session struct {
	files    map[string][]byte
	captured bool
}

func sessionAuthentication(name string) bool {
	name, _, _ = strings.Cut(name, "/")
	return name == "auth.json" || name == ".credentials.json"
}

func sessionFiles(ctx context.Context, source fs.FS) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if sessionAuthentication(name) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("session contains a non-regular file")
		}
		data, err := fs.ReadFile(source, name)
		if err == nil {
			files[name] = data
		}
		return err
	})
	return files, err
}

// Restore reads a retained copy and verifies Profile files against the newly
// prepared immutable environment. A missing/unreadable copy or missing pinned
// file is unavailable; an existing pinned file with changed bytes is a
// configuration error.
func (s *Session) Restore(ctx context.Context, source fs.FS, original *Environment, manifest map[string]string) error {
	files, err := sessionFiles(ctx, source)
	if err != nil {
		return err
	}
	for name, want := range original.expected {
		data, found := files[name]
		if !found {
			return fmt.Errorf("retained session files are incomplete")
		}
		if hash(string(data)) != want {
			return configError(fmt.Errorf("retained configuration differs from pinned file"))
		}
	}
	if len(files) != len(manifest) {
		return fmt.Errorf("retained session files are incomplete")
	}
	for name, want := range manifest {
		data, found := files[name]
		if !found || hash(string(data)) != want {
			return fmt.Errorf("retained session files are unavailable")
		}
	}
	s.files = files
	return nil
}

func (s *Session) install(ctx context.Context, destination *Environment) error {
	for name, data := range s.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if want, pinned := destination.expected[name]; pinned {
			if hash(string(data)) != want {
				return configError(fmt.Errorf("retained configuration differs from pinned file"))
			}
			continue
		}
		if err := destination.root.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		f, err := destination.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		// Seed and verify restored state too, without adding context revisions.
		destination.expected[name] = hash(string(data))
	}
	return nil
}

// CaptureSession reads a host-backed stopped copy. Container-backed copies use
// CaptureSessionArchive instead; their host staging directory is unchanged.
func (e *Environment) CaptureSession(ctx context.Context) error {
	if e.session == nil {
		return nil
	}
	if e.root == nil {
		return fmt.Errorf("configuration environment is closed")
	}
	if err := e.checkIdentity(); err != nil {
		return err
	}
	files, err := sessionFiles(ctx, e.root.FS())
	if err == nil {
		e.session.files, e.session.captured = files, true
	}
	return err
}

func (e *Environment) SessionEnabled() bool { return e.session != nil }
func (s *Session) Captured() bool           { return s.captured }

func (s *Session) Manifest() map[string]string {
	manifest := make(map[string]string, len(s.files))
	for name, data := range s.files {
		manifest[name] = hash(string(data))
	}
	return manifest
}

// DiscardSession invalidates a partial or unconfirmed container transfer.
func (e *Environment) DiscardSession() {
	if e.session != nil {
		e.session.files, e.session.captured = nil, false
	}
}

// CaptureSessionArchive accepts only a directory tree of regular files. It
// never extracts untrusted paths or links to the host and strips both auth names.
func (e *Environment) CaptureSessionArchive(ctx context.Context, source io.Reader) error {
	if e.session == nil {
		return nil
	}
	files := map[string][]byte{}
	seen := map[string]bool{}
	tr := tar.NewReader(source)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		if name == "" {
			name = "."
		}
		if !fs.ValidPath(name) || strings.ContainsAny(name, "\x00\\") {
			return fmt.Errorf("invalid session archive path")
		}
		if sessionAuthentication(name) {
			continue
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if name == "." || h.Typeflag != tar.TypeReg || seen[name] {
			return fmt.Errorf("invalid session archive entry")
		}
		seen[name] = true
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		files[name] = data
	}
	if _, err := io.Copy(io.Discard, source); err != nil {
		return err
	}
	e.session.files, e.session.captured = files, true
	return nil
}

// Save writes into a new, Runner-owned 0700 directory. Callers publish it only
// after all files have been written, and retain it only for a validated question.
func (s *Session) Save(ctx context.Context, destination *os.Root) error {
	if !s.captured {
		return fmt.Errorf("session was not captured")
	}
	for name, data := range s.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := destination.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		f, err := destination.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
