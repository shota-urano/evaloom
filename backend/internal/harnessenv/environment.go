// Package harnessenv prepares the immutable configuration mounted into a Harness
// Sandbox. HOME itself is an empty, per-container tmpfs owned by sandbox.Container.
package harnessenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
)

// Config is trusted Runner wiring, not a Profile or public API extension. Root
// must be a dedicated existing directory allowed by ContainerConfig.ConfigDirRoot.
// ConfigVariable is explicitly selected by the caller for its adopted CLI version.
// File paths are preserved: no CLI-specific relocation is inferred from versions.
type Config struct {
	Root, CommitSHA, ConfigVariable string
	Files                           []gitcontext.File
}

type Environment struct {
	directory, variable, commit string
	parent                      *os.Root
	root                        *os.Root
	expected                    map[string]string
}

func configError(err error) error { return &harnesscontext.ConfigError{Err: err} }

// Create never reads the user's HOME, a harness working tree, or ambient env.
// It writes only to its newly created directory and returns no partial result.
func Create(ctx context.Context, config Config) (_ *Environment, err error) {
	if err := ctx.Err(); err != nil {
		return nil, configError(err)
	}
	if !filepath.IsAbs(config.Root) || (config.ConfigVariable != "CODEX_HOME" && config.ConfigVariable != "CLAUDE_CONFIG_DIR") ||
		!commitSHA(config.CommitSHA) {
		return nil, configError(fmt.Errorf("explicit configuration root, CLI variable and pinned commit required"))
	}
	expected := make(map[string]string, len(config.Files))
	for _, file := range config.Files {
		if !fs.ValidPath(file.Path) || file.Path == "." || strings.ContainsAny(file.Path, "\x00\\") ||
			file.SourceCommitSHA != config.CommitSHA || file.ContentHash != hash(file.Content) {
			return nil, configError(fmt.Errorf("invalid pinned configuration file"))
		}
		if _, duplicate := expected[file.Path]; duplicate {
			return nil, configError(fmt.Errorf("duplicate configuration file"))
		}
		expected[file.Path] = file.ContentHash
	}
	parent, err := os.OpenRoot(config.Root)
	if err != nil {
		return nil, configError(err)
	}
	directory, err := os.MkdirTemp(config.Root, "harness-")
	if err != nil {
		parent.Close()
		return nil, configError(err)
	}
	e := &Environment{directory: directory, variable: config.ConfigVariable, commit: config.CommitSHA, parent: parent, expected: expected}
	defer func() {
		if err != nil {
			err = configError(fmt.Errorf("prepare configuration: %w", err))
			if cleanupErr := e.Close(); cleanupErr != nil {
				err = fmt.Errorf("%w; cleanup: %v", err, cleanupErr)
			}
		}
	}()
	if e.root, err = os.OpenRoot(directory); err != nil {
		return nil, err
	}
	for _, file := range config.Files {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if err = e.root.MkdirAll(filepath.FromSlash(path.Dir(file.Path)), 0700); err != nil {
			return nil, err
		}
		var out *os.File
		out, err = e.root.OpenFile(filepath.FromSlash(file.Path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		_, err = out.WriteString(file.Content)
		closeErr := out.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
	}
	if _, err = e.Files(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

func commitSHA(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && (len(s) == 40 || len(s) == 64) && strings.ToLower(s) == s
}

func hash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func (e *Environment) ConfigDir() string { return e.directory }

// Env contains only the selected CLI's configuration directory. The Sandbox
// supplies HOME and filters all other variables using its explicit allowlist.
func (e *Environment) Env() map[string]string { return map[string]string{e.variable: e.directory} }

// Files enumerates the actual directory, rejecting added, removed, changed or
// non-regular files. Hashes cover the bytes on disk, not a caller's assertion.
// The same repository-relative paths can be sent to the existing context API.
func (e *Environment) Files(ctx context.Context) ([]gitcontext.File, error) {
	if e.root == nil {
		return nil, configError(fmt.Errorf("configuration environment is closed"))
	}
	files := []gitcontext.File{}
	err := fs.WalkDir(e.root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("configuration contains a non-regular file")
		}
		want, found := e.expected[name]
		if !found {
			return fmt.Errorf("configuration contains an unselected file")
		}
		content, err := fs.ReadFile(e.root.FS(), name)
		if err != nil {
			return err
		}
		h := hash(string(content))
		if h != want {
			return fmt.Errorf("configuration content differs from pinned file")
		}
		files = append(files, gitcontext.File{Path: name, Content: string(content), ContentHash: h, SourceCommitSHA: e.commit})
		return nil
	})
	if err != nil {
		return nil, configError(err)
	}
	if len(files) != len(e.expected) {
		return nil, configError(fmt.Errorf("configuration file missing"))
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// Close removes only this Environment's generated directory. Call after the
// Sandbox has stopped, including on execution failure or cancellation.
func (e *Environment) Close() error {
	if e.root != nil {
		e.root.Close()
		e.root = nil
	}
	if e.parent == nil {
		return nil
	}
	if err := e.parent.RemoveAll(filepath.Base(e.directory)); err != nil {
		return err
	}
	err := e.parent.Close()
	e.parent = nil
	return err
}
