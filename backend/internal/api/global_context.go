package api

import (
	"context"
	"fmt"
	"io/fs"
)

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
	err := fs.WalkDir(s.Repository, "context/global", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
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
