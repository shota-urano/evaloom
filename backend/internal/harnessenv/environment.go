// Package harnessenv prepares the immutable configuration mounted into a Harness
// Sandbox. HOME itself is an empty, per-container tmpfs owned by sandbox.Container.
package harnessenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
// Source paths are preserved. Instructions optionally selects, in declaration
// order, the files to concatenate at the CLI's user-instruction path (spec 08 Q142).
type Config struct {
	Root, CommitSHA, ConfigVariable string
	Files                           []gitcontext.File
	Instructions                    []string
}

type Environment struct {
	directory, variable, commit string
	parent                      *os.Root
	root                        *os.Root
	expected                    map[string]string
	working                     bool
	authentication              *Authentication
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
		if file.Path == "auth.json" || file.Path == ".credentials.json" {
			return nil, configError(fmt.Errorf("authentication files cannot be pinned configuration"))
		}
		if !fs.ValidPath(file.Path) || file.Path == "." || strings.ContainsAny(file.Path, "\x00\\") ||
			file.SourceCommitSHA != config.CommitSHA || file.ContentHash != hash(file.Content) {
			return nil, configError(fmt.Errorf("invalid pinned configuration file"))
		}
		if _, duplicate := expected[file.Path]; duplicate {
			return nil, configError(fmt.Errorf("duplicate configuration file"))
		}
		expected[file.Path] = file.ContentHash
	}
	if len(config.Instructions) > 0 {
		instruction, err := composeInstructions(config)
		if err != nil {
			return nil, configError(err)
		}
		if existing, found := expected[instruction.Path]; found {
			if existing != instruction.ContentHash {
				return nil, configError(fmt.Errorf("user instruction path conflicts with selected configuration"))
			}
		} else {
			// Own the slice before appending; the caller's snapshots stay intact.
			config.Files = append(append([]gitcontext.File{}, config.Files...), instruction)
			expected[instruction.Path] = instruction.ContentHash
		}
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

func composeInstructions(config Config) (gitcontext.File, error) {
	files := append([]gitcontext.File{}, config.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	seen := map[string]bool{}
	var content strings.Builder
	for _, declaration := range config.Instructions {
		name := path.Clean(declaration)
		if !fs.ValidPath(name) || name == "." || strings.ContainsAny(declaration, "\x00\\") {
			return gitcontext.File{}, fmt.Errorf("invalid instruction declaration")
		}
		for _, part := range strings.Split(declaration, "/") {
			if part == ".." {
				return gitcontext.File{}, fmt.Errorf("invalid instruction declaration")
			}
		}
		found := false
		for _, file := range files {
			if file.Path != name && !strings.HasPrefix(file.Path, name+"/") {
				continue
			}
			found = true
			if !seen[file.Path] {
				seen[file.Path] = true
				content.WriteString(file.Content)
			}
		}
		if !found {
			return gitcontext.File{}, fmt.Errorf("declared instruction is missing from selected configuration")
		}
	}
	name := "AGENTS.md"
	if config.ConfigVariable == "CLAUDE_CONFIG_DIR" {
		name = "CLAUDE.md"
	}
	return gitcontext.File{Path: name, Content: content.String(), ContentHash: hash(content.String()), SourceCommitSHA: config.CommitSHA}, nil
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

// WorkingCopy makes an independent, disposable sibling under the same trusted
// configuration root. The expected paths/hashes come from this Environment,
// including its Q142 concatenation, never from the copy itself. Runner submits
// the source files to context_revisions before the Adapter requests a copy.
// The caller owns Close, and must verify Files again before the first CLI starts.
func (e *Environment) WorkingCopy(ctx context.Context) (*Environment, error) {
	files, err := e.Files(ctx)
	if err != nil {
		return nil, err
	}
	copy, err := Create(ctx, Config{Root: filepath.Dir(e.directory), CommitSHA: e.commit,
		ConfigVariable: e.variable, Files: files})
	if err == nil {
		copy.working = true
	}
	return copy, err
}

// Files enumerates pinned configuration, rejecting added, removed, changed or
// non-regular files. The installed authentication file is checked for presence
// and type only; neither its path nor its content/hash enters the returned list.
// Hashes cover configuration bytes on disk, not a caller's assertion.
// Runner reports only source paths to the context API (spec 07 section 5); a
// Q142 concatenation is verified here but has no separate source revision row.
func (e *Environment) Files(ctx context.Context) ([]gitcontext.File, error) {
	if e.root == nil {
		return nil, configError(fmt.Errorf("configuration environment is closed"))
	}
	// Never silently read a retained descriptor after its published name has
	// been replaced. Lstat also rejects a symlink back to the original config.
	if err := e.checkIdentity(); err != nil {
		return nil, configError(err)
	}
	files := []gitcontext.File{}
	authSeen := false
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
		if e.authentication != nil && name == authenticationName(e.variable) {
			authSeen = true
			return nil
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
	if e.authentication != nil && !authSeen {
		return nil, configError(fmt.Errorf("authentication file missing from copy"))
	}
	if len(files) != len(e.expected) {
		return nil, configError(fmt.Errorf("configuration file missing"))
	}
	if err := e.checkIdentity(); err != nil {
		return nil, configError(err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func (e *Environment) checkIdentity() error {
	actual, err := e.root.Stat(".")
	if err != nil {
		return err
	}
	named, err := os.Lstat(e.directory)
	if err != nil {
		return err
	}
	if !named.IsDir() || !os.SameFile(actual, named) {
		return fmt.Errorf("configuration directory replaced")
	}
	return nil
}

// Close removes only this Environment's generated directory. Call after the
// Sandbox has stopped, including on execution failure or cancellation.
func (e *Environment) Close() error {
	if e.parent == nil {
		return nil
	}
	var cleanup error
	if e.root != nil {
		identity, err := e.root.Stat(".")
		cleanup = errors.Join(cleanup, err)
		// Clear through the retained handle, even if the directory was moved.
		entries, err := fs.ReadDir(e.root.FS(), ".")
		cleanup = errors.Join(cleanup, err)
		for _, entry := range entries {
			cleanup = errors.Join(cleanup, e.root.RemoveAll(entry.Name()))
		}
		if identity != nil {
			// Find the generated inode under its trusted root rather than delete
			// an unrelated replacement at its former name. Never follow links.
			named, statErr := e.parent.Lstat(filepath.Base(e.directory))
			if statErr == nil && os.SameFile(identity, named) {
				err = e.parent.Remove(filepath.Base(e.directory))
			} else {
				err = fs.WalkDir(e.parent.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
					if os.IsNotExist(err) {
						return nil
					} // another execution finished
					if err != nil {
						return err
					}
					if name == "." || !entry.IsDir() {
						return nil
					}
					info, err := entry.Info()
					if os.IsNotExist(err) {
						return nil
					}
					if err != nil {
						return err
					}
					if os.SameFile(identity, info) {
						if err := e.parent.Remove(name); err != nil {
							return err
						}
						return fs.SkipDir
					}
					return nil
				})
			}
			cleanup = errors.Join(cleanup, err)
		}
		cleanup = errors.Join(cleanup, e.root.Close())
		e.root = nil
	}
	cleanup = errors.Join(cleanup, e.parent.Close())
	e.parent = nil
	return cleanup
}
