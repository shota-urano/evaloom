package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"gopkg.in/yaml.v3"
)

var profileVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?(\+([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?$`)
var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

const profileDefinitionPath = "evaloom/profile.yaml"

// Git tree modes (contract 4.1): only regular files and directories can be
// referenced. Symbolic links (120000) and submodules (160000) cannot.
const (
	modeFile       = "100644"
	modeExecutable = "100755"
	modeTree       = "040000"
	modeSymlink    = "120000"
	modeSubmodule  = "160000"
)

func ValidProfileVersion(version string) bool {
	if !profileVersionPattern.MatchString(version) {
		return false
	}
	core, _, _ := strings.Cut(version, "+")
	_, prerelease, _ := strings.Cut(core, "-")
	for _, part := range strings.Split(prerelease, ".") {
		if len(part) > 1 && part[0] == '0' && strings.Trim(part, "0123456789") == "" {
			return false
		}
	}
	return true
}

// ReadProfile resolves the exact tag profile/v<version>, reads
// evaloom/profile.yaml from that commit (never the working tree) and checks it
// against spec 03 3.4. Every failed rule is returned in one
// PreconditionError. After the tag resolves, CommitSHA is set even on
// definition errors, so the caller can give an existing registration
// precedence. The entire operation shares the repository Git deadline, and the
// caller must perform it before opening a DB transaction.
func ReadProfile(parent context.Context, repo, version string, environment contract.ProfileEnvironment) (model.HarnessProfile, error) {
	p := model.HarnessProfile{Version: version, TagName: "profile/v" + version}
	if !ValidProfileVersion(version) {
		return p, rejected("version", "invalid_format")
	}
	ctx, cancel := context.WithTimeout(parent, repositoryValidationTimeout)
	defer cancel()
	sha, err := repositoryGit(ctx, repo, "rev-parse", "--verify", "--end-of-options", "refs/tags/"+p.TagName+"^{commit}")
	if err != nil {
		return p, gitError(parent, "tagName", "not_found")
	}
	sha = strings.TrimSuffix(sha, "\n")
	if !commitSHAPattern.MatchString(sha) {
		return p, rejected("commitSha", "invalid_format")
	}
	p.CommitSHA = sha
	tree, err := commitTree(ctx, repo, sha)
	if err != nil {
		return p, gitError(parent, "commitSha", "not_found")
	}
	if mode := tree[profileDefinitionPath]; mode != modeFile && mode != modeExecutable {
		return p, rejected("definition", "not_found")
	}
	data, err := repositoryGit(ctx, repo, "cat-file", "blob", sha+":"+profileDefinitionPath)
	if err != nil {
		return p, gitError(parent, "definition", "not_found")
	}
	encoded, err := profileJSON([]byte(data))
	if err != nil {
		return p, rejected("definition", "invalid_format")
	}
	def, found := contract.DecodeProfile(encoded)
	found = append(found, def.EnvironmentIssues(version, environment)...)
	found = append(found, referenceIssues(tree, def)...)
	if schema := schemaPath(def, found); schema != "" {
		if issue, err := schemaIssue(ctx, repo, sha, schema); err != nil {
			if ctxErr := parent.Err(); ctxErr != nil {
				return p, ctxErr
			}
			return p, err
		} else if issue != "" {
			found = append(found, contract.Issue{Path: "params.schema", Reason: issue})
		}
	}
	if err := parent.Err(); err != nil {
		return p, err
	}
	if len(found) > 0 {
		return p, profileRejected(found)
	}
	hash := sha256.Sum256(encoded)
	p.Definition, p.DefinitionHash = encoded, hex.EncodeToString(hash[:])
	p.ContractVersion, p.ModelID = def.ContractVersion, def.Stages.Work.Model
	return p, nil
}

// profileJSON converts exactly one YAML document to key-sorted JSON. YAML
// comments and formatting are not part of the stored definition.
func profileJSON(data []byte) ([]byte, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document map[string]any
	if err := decoder.Decode(&document); err != nil || document == nil {
		return nil, fmt.Errorf("profile: not a YAML mapping")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("profile: multiple YAML documents")
	}
	return json.Marshal(document)
}

// commitTree lists every entry of the commit with its Git mode.
func commitTree(ctx context.Context, repo, sha string) (map[string]string, error) {
	out, err := repositoryGit(ctx, repo, "ls-tree", "-r", "-t", "-z", "--full-tree", "--end-of-options", sha)
	if err != nil {
		return nil, err
	}
	tree := map[string]string{}
	for _, entry := range strings.Split(out, "\x00") {
		meta, name, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		mode, _, _ := strings.Cut(meta, " ")
		tree[name] = mode
	}
	return tree, nil
}

// referenceIssues checks that each referenced path exists in the commit and is
// neither a symbolic link nor a submodule (spec 03 3.4 参照するパス). Commands
// and params.schema must be files; work files may also be directories whose
// contents contain no symbolic links or submodules.
func referenceIssues(tree map[string]string, def contract.ProfileDefinition) []contract.Issue {
	var found []contract.Issue
	for _, ref := range def.ReferencedPaths() {
		if ref.Value == "" || path.IsAbs(ref.Value) || strings.ContainsAny(ref.Value, "\\\x00") ||
			slices.Contains(strings.Split(ref.Value, "/"), "..") {
			continue // already reported as outside_repository
		}
		name := path.Clean(ref.Value)
		directoryAllowed := strings.HasPrefix(ref.Path, "stages.work.")
		switch mode, ok := tree[name]; {
		case !ok:
			found = append(found, contract.Issue{Path: ref.Path, Reason: "not_found"})
		case mode == modeSymlink:
			found = append(found, contract.Issue{Path: ref.Path, Reason: "symlink"})
		case mode == modeSubmodule:
			found = append(found, contract.Issue{Path: ref.Path, Reason: "submodule"})
		case mode == modeTree && !directoryAllowed:
			found = append(found, contract.Issue{Path: ref.Path, Reason: "not_a_file"})
		case mode == modeTree:
			for entry, entryMode := range tree {
				if strings.HasPrefix(entry, name+"/") && (entryMode == modeSymlink || entryMode == modeSubmodule) {
					reason := "contains_symlink"
					if entryMode == modeSubmodule {
						reason = "contains_submodule"
					}
					found = append(found, contract.Issue{Path: ref.Path, Reason: reason})
					break
				}
			}
		case mode != modeFile && mode != modeExecutable:
			found = append(found, contract.Issue{Path: ref.Path, Reason: "not_a_file"})
		}
	}
	return found
}

// ValidateProfileReferences reuses registration's commit-tree checks for Runner
// preflight. In particular, git archive represents submodules as empty directories,
// so checking only the extracted filesystem would miss them.
func ValidateProfileReferences(ctx context.Context, repo, sha string, def contract.ProfileDefinition) error {
	tree, err := commitTree(ctx, repo, sha)
	if err != nil {
		return err
	}
	if found := referenceIssues(tree, def); len(found) > 0 {
		return profileRejected(found)
	}
	return nil
}

// schemaPath returns params.schema when it is a valid committed file.
func schemaPath(def contract.ProfileDefinition, found []contract.Issue) string {
	if def.Params == nil || def.Params.Schema == "" {
		return ""
	}
	for _, issue := range found {
		if issue.Path == "params.schema" {
			return ""
		}
	}
	return path.Clean(def.Params.Schema)
}

// schemaIssue compiles params.schema as JSON Schema draft 2020-12 from the
// commit. References can only reach files of the same commit.
func schemaIssue(ctx context.Context, repo, sha, schema string) (string, error) {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(commitLoader{ctx: ctx, repo: repo, commit: sha})
	if _, err := compiler.Compile(schemaScheme + ":///" + schema); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "invalid_schema", nil
	}
	return "", nil
}

func profileRejected(found []contract.Issue) error {
	details := make([]Detail, 0, len(found))
	seen := map[string]bool{}
	for _, issue := range found {
		if seen[issue.Path] {
			continue
		}
		seen[issue.Path] = true
		details = append(details, Detail{Field: issue.Path, Reason: issue.Reason})
	}
	return &PreconditionError{
		Code:    "precondition_failed",
		Message: fmt.Sprintf("Profile definition does not satisfy the harness contract (%d issues)", len(details)),
		Details: details,
	}
}
