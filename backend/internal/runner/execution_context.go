package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
)

type workContext struct {
	Instructions string `json:"instructions"`
	Records      []struct {
		FileName   string `json:"fileName"`
		ArtifactID string `json:"artifactId"`
	} `json:"records"`
	RevisionIDs []string `json:"revisionIds"`
}

func (e *executionExecutor) pinWorkContext(ctx context.Context, in StageInvocation, files []harnesscontext.File) (workContext, error) {
	type contextFile struct {
		Kind   string `json:"kind"`
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	selected := []contextFile{}
	seen := map[string]bool{}
	for _, group := range []struct {
		kind  string
		paths []string
	}{{"instructions", in.Profile.Definition.Stages.Work.Instructions}, {"skill", in.Profile.Definition.Stages.Work.Skills}, {"settings", in.Profile.Definition.Stages.Work.Settings}} {
		for _, declaration := range group.paths {
			name := path.Clean(declaration)
			for _, file := range files {
				key := group.kind + "\x00" + file.Path
				if (file.Path == name || strings.HasPrefix(file.Path, name+"/")) && !seen[key] {
					seen[key] = true
					selected = append(selected, contextFile{group.kind, file.Path, file.ContentHash})
				}
			}
		}
	}
	body, _ := json.Marshal(struct {
		Commit string        `json:"profileCommitSha"`
		Files  []contextFile `json:"files"`
	}{e.claim.Profile.CommitSHA, selected})
	endpoint, _ := url.JoinPath(e.runner.controlPlaneURL, "api", "executions", e.claim.Execution.ID, "context")
	request, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return workContext{}, err
	}
	request.Header.Set("Authorization", "Bearer "+e.runner.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := e.runner.client.Do(request)
	if err != nil {
		return workContext{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return workContext{}, fmt.Errorf("runner: context returned HTTP %d", response.StatusCode)
	}
	var reply workContext
	err = json.NewDecoder(response.Body).Decode(&reply)
	// Retrieval of record files has no Runner API in the confirmed API table.
	// Do not invent one, or silently execute without the selected records.
	if err == nil && len(reply.Records) > 0 {
		err = fmt.Errorf("runner: record artifact retrieval is not specified")
	}
	return reply, err
}
