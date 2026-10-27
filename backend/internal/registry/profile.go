package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"gopkg.in/yaml.v3"
)

var profileVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?(\+([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?$`)
var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

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

// ReadProfile resolves only the exact tag and reads the committed blob, never
// the working tree. The entire operation shares the existing Git deadline.
// After successful tag resolution, CommitSHA is returned even on definition
// errors so the caller can give an existing immutable registration precedence.
// The caller must perform all Git work before opening a DB transaction.
func ReadProfile(parent context.Context, repo, version, adapter string) (model.HarnessProfile, error) {
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
	definition, err := repositoryGit(ctx, repo, "cat-file", "blob", sha+":evaloom/profile.yaml")
	if err != nil {
		return p, gitError(parent, "definition", "not_found")
	}
	p.Definition, p.ModelID, err = validateProfileDefinition([]byte(definition), version, adapter)
	if err != nil {
		return p, err
	}
	hash := sha256.Sum256(p.Definition)
	p.DefinitionHash = hex.EncodeToString(hash[:])
	return p, nil
}

func validateProfileDefinition(data []byte, version, adapter string) (json.RawMessage, string, error) {
	// Decode through YAML's generic representation before the typed JSON schema:
	// YAML's permissive string coercions must not turn numbers into valid strings.
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document map[string]any
	if err := decoder.Decode(&document); err != nil || document == nil {
		return nil, "", rejected("definition", "invalid_format")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, "", rejected("definition", "invalid_format")
	}
	// encoding/json matches struct fields case-insensitively, even with
	// DisallowUnknownFields. Reject aliases at every object level before decoding
	// so the values we validate cannot differ from the definition we store.
	if !validProfileDefinitionKeys(document) {
		return nil, "", rejected("definition", "invalid_format")
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, "", rejected("definition", "invalid_format")
	}
	var def model.ProfileDefinition
	jsonDecoder := json.NewDecoder(bytes.NewReader(encoded))
	jsonDecoder.DisallowUnknownFields()
	if err := jsonDecoder.Decode(&def); err != nil {
		return nil, "", rejected("definition", "invalid_format")
	}
	for _, value := range []string{def.ProfileVersion, def.HarnessVersion, def.ModelID, def.PromptVersion,
		def.ContextPolicyVersion, def.ExecutionConfiguration.Adapter} {
		if strings.TrimSpace(value) == "" {
			return nil, "", rejected("definition", "invalid_format")
		}
	}
	for _, skill := range def.SkillVersions {
		if strings.TrimSpace(skill.Name) == "" || strings.TrimSpace(skill.Version) == "" {
			return nil, "", rejected("definition", "invalid_format")
		}
	}
	for _, tool := range def.ToolConfiguration.AllowedTools {
		if strings.TrimSpace(tool) == "" {
			return nil, "", rejected("definition", "invalid_format")
		}
	}
	_, evaluationPresent := document["evaluationPolicyVersion"]
	if def.SkillVersions == nil || def.ToolConfiguration.AllowedTools == nil || def.ExecutionConfiguration.Args == nil ||
		def.CollaborationPolicy != "solo" || !evaluationPresent || def.EvaluationPolicyVersion != nil {
		return nil, "", rejected("definition", "invalid_format")
	}
	if def.ProfileVersion != version {
		return nil, "", rejected("profileVersion", "invalid")
	}
	if def.ExecutionConfiguration.Adapter != adapter {
		return nil, "", rejected("executionConfiguration.adapter", "invalid")
	}
	return encoded, def.ModelID, nil
}

func validProfileDefinitionKeys(document map[string]any) bool {
	if !onlyProfileKeys(document,
		"profileVersion", "harnessVersion", "modelId", "promptVersion", "skillVersions",
		"toolConfiguration", "contextPolicyVersion", "executionConfiguration",
		"collaborationPolicy", "evaluationPolicyVersion") {
		return false
	}
	if !onlyProfileKeys(document["toolConfiguration"], "allowedTools") ||
		!onlyProfileKeys(document["executionConfiguration"], "adapter", "args") {
		return false
	}
	skills, ok := document["skillVersions"].([]any)
	if !ok {
		return false
	}
	for _, skill := range skills {
		if !onlyProfileKeys(skill, "name", "version") {
			return false
		}
	}
	return true
}

func onlyProfileKeys(value any, allowed ...string) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for key := range object {
		if !slices.Contains(allowed, key) {
			return false
		}
	}
	return true
}
