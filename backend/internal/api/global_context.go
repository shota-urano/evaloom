package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/registry"
)

// globalContextDir is the only Global source (Q140): every regular file under
// it goes to every work stage.
const globalContextDir = "context/global"

// RepositoryGlobalContext selects Q140's common files only. The composition
// root must supply the Evaloom repository filesystem and its revision: their
// deployment location/version scheme is not specified by spec 07.
type RepositoryGlobalContext struct {
	Repository fs.FS
	Revision   string
}

func (s RepositoryGlobalContext) ReadGlobalContext(ctx context.Context) ([]GlobalContextFile, error) {
	if s.Repository == nil || s.Revision == "" {
		return nil, fmt.Errorf("Global Context requires a repository and revision")
	}
	files := []GlobalContextFile{}
	err := fs.WalkDir(s.Repository, globalContextDir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A missing context/global/ is zero files, not an error (Q207).
			if name == globalContextDir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		// Do not follow links into destination-specific or workspace files.
		if !entry.Type().IsRegular() {
			return fmt.Errorf("Global Context contains a non-regular file: %s", name)
		}
		content, err := fs.ReadFile(s.Repository, name)
		if err != nil {
			return err
		}
		files = append(files, GlobalContextFile{Path: name, Revision: s.Revision, Content: content})
		return nil
	})
	return files, err
}

// CommitGlobalContext reads context/global/ from one pinned commit of the
// Evaloom repository (spec 07 3.5, Q206). The working tree, the index and later
// commits are never read, so uncommitted edits cannot reach a work stage.
type CommitGlobalContext struct {
	Repository string
	Commit     string
}

// NewCommitGlobalContext pins the repository's current HEAD commit. evaloomd
// calls it once at startup; the SHA becomes every global row's source_revision.
// repository must be the absolute path of a Git repository root, checked like
// a Harness repository path, so a subdirectory never resolves a parent's HEAD.
func NewCommitGlobalContext(ctx context.Context, repository string) (CommitGlobalContext, error) {
	root, err := registry.ValidateRepository(ctx, registry.RepositoryInput{Path: repository, SkipRemote: true})
	if err != nil {
		return CommitGlobalContext{}, err
	}
	commit, err := gitcontext.HeadCommitSHA(ctx, root)
	if err != nil {
		return CommitGlobalContext{}, err
	}
	return CommitGlobalContext{Repository: root, Commit: commit}, nil
}

func (s CommitGlobalContext) ReadGlobalContext(ctx context.Context) ([]GlobalContextFile, error) {
	if s.Repository == "" || s.Commit == "" {
		return nil, fmt.Errorf("Global Context requires a repository and commit")
	}
	// Git tracks no empty directories, so an absent or empty context/global/
	// yields zero files (Q207). Links and submodules are rejected by Read.
	snapshot, err := gitcontext.Read(ctx, s.Repository, s.Commit, func(name string) bool {
		return strings.HasPrefix(name, globalContextDir+"/")
	})
	if err != nil {
		return nil, err
	}
	files := make([]GlobalContextFile, 0, len(snapshot))
	for _, file := range snapshot {
		files = append(files, GlobalContextFile{Path: file.Path, Revision: file.SourceCommitSHA, Content: []byte(file.Content)})
	}
	return files, nil
}
