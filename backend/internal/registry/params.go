package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaScheme names files inside the recorded commit. Relative $ref values
// resolve against it, so a schema can only reach files of the same commit.
const schemaScheme = "commit"

// ValidateProfileParams checks params with the Profile's params.schema read
// from commitSHA in repo (contract 5.2, draft 2020-12). It never reads the
// working tree or the network. All Git work happens here, so callers must run
// it before opening a DB transaction.
func ValidateProfileParams(parent context.Context, repo, commitSHA, schemaPath string, params json.RawMessage) error {
	if !commitSHAPattern.MatchString(commitSHA) || !committedPath(schemaPath) {
		return rejected("params.schema", "invalid_format")
	}
	ctx, cancel := context.WithTimeout(parent, repositoryValidationTimeout)
	defer cancel()
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(commitLoader{ctx: ctx, repo: repo, commit: commitSHA})
	schema, err := compiler.Compile(schemaScheme + ":///" + schemaPath)
	if err != nil {
		if ctxErr := parent.Err(); ctxErr != nil {
			return ctxErr
		}
		return rejected("params.schema", "invalid")
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(params))
	if err != nil {
		return rejected("params", "invalid_format")
	}
	if schema.Validate(instance) != nil {
		return rejected("params", "invalid")
	}
	return nil
}

type commitLoader struct {
	ctx    context.Context
	repo   string
	commit string
}

func (l commitLoader) Load(location string) (any, error) {
	u, err := url.Parse(location)
	if err != nil || u.Scheme != schemaScheme || u.Host != "" || u.RawQuery != "" {
		return nil, errors.New("schema reference outside the recorded commit")
	}
	name := strings.TrimPrefix(u.Path, "/")
	if !committedPath(name) {
		return nil, errors.New("schema reference outside the recorded commit")
	}
	// Symbolic links (120000) and submodules (160000) are not file contents
	// of the commit (contract 4.1).
	entry, err := repositoryGit(l.ctx, l.repo, "ls-tree", "--end-of-options", l.commit, "--", name)
	if err != nil {
		return nil, err
	}
	if mode, _, _ := strings.Cut(entry, " "); mode != "100644" && mode != "100755" {
		return nil, fmt.Errorf("schema reference is not a regular file")
	}
	blob, err := repositoryGit(l.ctx, l.repo, "cat-file", "blob", l.commit+":"+name)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(strings.NewReader(blob))
}

func committedPath(name string) bool {
	return name != "" && name != "." && !path.IsAbs(name) && path.Clean(name) == name &&
		name != ".." && !strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\x00\\")
}
