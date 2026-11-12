package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/registry"
	"github.com/shota-urano/evaloom/backend/internal/safegit"
)

var profileCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ProfileConfigError is safe to report without repository paths or Git output.
type ProfileConfigError struct{}

func (*ProfileConfigError) Error() string                 { return "runner: cannot read fixed Profile content" }
func (*ProfileConfigError) Status() model.ExecutionStatus { return model.ExecutionFailed }
func (*ProfileConfigError) ErrorClass() model.ErrorClass  { return model.ErrorConfig }

// LoadedProfile is the fixed tree for program-stage read-only mounts. Work-stage
// file projection belongs to the work configuration layer (spec 05 3.4 step 4).
type LoadedProfile struct {
	Path       string
	Definition contract.ProfileDefinition
}

// WithProfile is the Profile preflight boundary for the stage framework. The
// caller must put all stage starts inside start and report classified errors.
// It does not start stages, create sandboxes or define a new reporting protocol.
func (runner *Runner) WithProfile(ctx context.Context, claim Claim, start func(context.Context, LoadedProfile) error) error {
	if start == nil {
		return fmt.Errorf("runner: profile start function is required")
	}
	loaded, err := runner.loadProfile(ctx, claim)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &ProfileConfigError{}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return start(ctx, loaded)
}

func (runner *Runner) loadProfile(ctx context.Context, claim Claim) (LoadedProfile, error) {
	runner.profileMu.Lock()
	defer runner.profileMu.Unlock()
	fail := func() (LoadedProfile, error) { return LoadedProfile{}, &ProfileConfigError{} }
	p := claim.Profile
	if !profileCommitPattern.MatchString(p.CommitSHA) || !registry.ValidProfileVersion(p.Version) ||
		p.TagName != "profile/v"+p.Version || !filepath.IsAbs(p.RepoPath) || !filepath.IsAbs(runner.dataDir) {
		return fail()
	}
	def, issues := contract.DecodeProfile(p.Definition)
	if len(issues) > 0 {
		return fail()
	}
	// A missing tag is allowed; a present but unreadable/non-commit tag is not.
	// Repeat this check even when the commit already has an extracted cache.
	ref := "refs/tags/" + p.TagName
	if _, err := safegit.Output(ctx, p.RepoPath, "show-ref", "--verify", "--quiet", ref); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return fail()
		}
	} else {
		sha, err := safegit.Output(ctx, p.RepoPath, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
		if err != nil || strings.TrimSuffix(string(sha), "\n") != p.CommitSHA {
			return fail()
		}
	}
	sha, err := safegit.Output(ctx, p.RepoPath, "rev-parse", "--verify", "--end-of-options", p.CommitSHA+"^{commit}")
	if err != nil || strings.TrimSuffix(string(sha), "\n") != p.CommitSHA {
		return fail()
	}
	if err := registry.ValidateProfileReferences(ctx, p.RepoPath, p.CommitSHA, def); err != nil {
		return fail()
	}
	if err := os.MkdirAll(runner.dataDir, 0700); err != nil {
		return fail()
	}
	data, err := os.OpenRoot(runner.dataDir)
	if err != nil {
		return fail()
	}
	defer data.Close()
	if err := data.Mkdir("profiles", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return fail()
	}
	if info, err := data.Lstat("profiles"); err != nil || !info.IsDir() {
		return fail()
	}
	profiles, err := data.OpenRoot("profiles")
	if err != nil {
		return fail()
	}
	defer profiles.Close()
	loaded := LoadedProfile{Path: filepath.Join(runner.dataDir, "profiles", p.CommitSHA), Definition: def}
	if _, err := profiles.Lstat(p.CommitSHA); err == nil {
		if err := validateExtractedProfile(profiles, p.CommitSHA, def); err != nil {
			return fail()
		}
		return loaded, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail()
	}
	archive, err := archiveProfile(ctx, p.RepoPath, p.CommitSHA, filepath.Join(runner.dataDir, "profiles"))
	if err != nil {
		return fail()
	}
	// Publish only a complete read-only tree. Serializing within the Runner keeps
	// its two job slots from replacing or modifying a tree another job uses.
	tmp := ".extract-" + ulid.Make().String()
	if err := profiles.Mkdir(tmp, 0700); err != nil {
		return fail()
	}
	defer func() {
		// Only this invocation's unpublished temporary tree is removed. Restore
		// directory permissions first if validation failed after freezing it.
		_ = fs.WalkDir(profiles.FS(), tmp, func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return profiles.Chmod(name, 0700)
			}
			return err
		})
		_ = profiles.RemoveAll(tmp)
	}()
	root, err := profiles.OpenRoot(tmp)
	if err != nil {
		return fail()
	}
	err = extractProfile(ctx, root, archive)
	root.Close()
	if err != nil {
		return LoadedProfile{}, fmt.Errorf("extract: %w", err)
	}
	if err := freezeProfile(profiles, tmp); err != nil {
		return LoadedProfile{}, fmt.Errorf("freeze: %w", err)
	}
	if err := validateExtractedProfile(profiles, tmp, def); err != nil {
		return LoadedProfile{}, fmt.Errorf("validate: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return LoadedProfile{}, err
	}
	if err := profiles.Rename(tmp, p.CommitSHA); err != nil {
		return fail()
	}
	return loaded, nil
}

// Archive through a private bare repository that borrows only the source object
// database. In particular, the source's info/attributes and configuration must
// not affect the fixed commit. No refs, index or working tree are copied.
func archiveProfile(ctx context.Context, repo, commit, profiles string) ([]byte, error) {
	objects, err := safegit.Output(ctx, repo, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, err
	}
	view, err := os.MkdirTemp(profiles, ".archive-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(view)
	if _, err := safegit.Output(ctx, view, "init", "--bare", "--object-format=sha1"); err != nil {
		return nil, err
	}
	if err := os.Symlink(strings.TrimSuffix(string(objects), "\n"), filepath.Join(view, "source-objects")); err != nil {
		return nil, err
	}
	// A relative alternate through our symlink handles source paths containing
	// newlines or quotes without encoding them in Git's alternates file format.
	if err := os.WriteFile(filepath.Join(view, "objects", "info", "alternates"), []byte("../source-objects\n"), 0600); err != nil {
		return nil, err
	}
	return safegit.Output(ctx, view, "archive", "--format=tar", commit)
}

// Delay symlinks until all writes finish, so no archive entry can redirect a
// later write. os.Root also confines every operation to the temporary tree.
func extractProfile(ctx context.Context, root *os.Root, archive []byte) error {
	reader := tar.NewReader(bytes.NewReader(archive))
	var links []tar.Header
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue // git archive records the commit ID in a global PAX header.
		}
		name := strings.TrimSuffix(header.Name, "/")
		if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") {
			return fmt.Errorf("invalid archive entry %q", header.Name)
		}
		if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0700); err != nil {
				return err
			}
		case tar.TypeReg:
			file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600|fs.FileMode(header.Mode)&0100)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			links = append(links, *header)
		default:
			return fmt.Errorf("unsupported archive type %d for %q", header.Typeflag, header.Name)
		}
	}
	for _, link := range links {
		if err := root.Symlink(link.Linkname, link.Name); err != nil {
			return err
		}
	}
	return nil
}

func freezeProfile(profiles *os.Root, name string) error {
	return fs.WalkDir(profiles.FS(), name, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.Type()&os.ModeSymlink != 0 {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := fs.FileMode(0400) | info.Mode().Perm()&0100
		if entry.IsDir() {
			mode = 0500
		}
		return profiles.Chmod(name, mode)
	})
}

func validateExtractedProfile(profiles *os.Root, name string, def contract.ProfileDefinition) error {
	info, err := profiles.Lstat(name)
	if err != nil || !info.IsDir() {
		return &ProfileConfigError{}
	}
	root, err := profiles.OpenRoot(name)
	if err != nil {
		return err
	}
	defer root.Close()
	// Reject a writable cache instead of changing content already in use.
	if err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			// Program stages see the entire tree, including unreferenced links.
			// Resolve through os.Root so absolute paths, parent escapes and
			// chains cannot reach outside the fixed commit. Dangling/cyclic
			// links also fail closed. Never stat through a host absolute path.
			if _, err := root.Stat(name); err != nil {
				return &ProfileConfigError{}
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0222 != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return &ProfileConfigError{}
		}
		if info.Mode().Perm()&0400 == 0 || (info.IsDir() && info.Mode().Perm()&0100 == 0) {
			return &ProfileConfigError{}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, ref := range def.ReferencedPaths() {
		// Inspect each component without following links, including links that
		// point inside the cache. Referenced directories include all descendants.
		var current string
		for _, part := range strings.Split(path.Clean(ref.Value), "/") {
			current = path.Join(current, part)
			info, err = root.Lstat(current)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				return &ProfileConfigError{}
			}
		}
		if !strings.HasPrefix(ref.Path, "stages.work.") && !info.Mode().IsRegular() {
			return &ProfileConfigError{}
		}
		if err := fs.WalkDir(root.FS(), current, func(_ string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && !entry.Type().IsRegular() {
				return &ProfileConfigError{}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
