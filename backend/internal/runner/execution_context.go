package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/oklog/ulid/v2"

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
	return reply, err
}

// placeWorkRecords fetches each pinned record through the Runner API
// (spec 07 3.3 step 3, Q208) and places it read-only in the stage's records/
// directory, which the work Sandbox mounts read-only (spec 09). A record whose
// file is missing (HTTP 404) is skipped and noted in the Runner log; records
// are reference material, so work continues (spec 07 6). Any other failure is
// returned so the stage does not silently run on a partial snapshot.
func (e *executionExecutor) placeWorkRecords(ctx context.Context, in StageInvocation, reply workContext, log *Log) error {
	directory := filepath.Join(in.Directory, "records")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, record := range reply.Records {
		// The Control Plane names records with one file name (contract 9).
		if record.FileName == "" || record.FileName == "." || record.FileName == ".." ||
			strings.ContainsAny(record.FileName, "/\\\x00") {
			return fmt.Errorf("runner: invalid record file name")
		}
		if id, err := ulid.ParseStrict(record.ArtifactID); err != nil || id.String() != record.ArtifactID {
			return fmt.Errorf("runner: invalid record artifact ID")
		}
		endpoint, _ := url.JoinPath(e.runner.controlPlaneURL, "api", "executions", e.claim.Execution.ID, "records", record.ArtifactID)
		request, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+e.runner.token)
		response, err := e.runner.client.Do(request)
		if err != nil {
			return err
		}
		if response.StatusCode == http.StatusNotFound {
			response.Body.Close()
			if log != nil {
				if err := log.Read(strings.NewReader("evaloom-runner: record " + record.FileName + " (artifact " + record.ArtifactID + ") is missing; not placed in records/\n")); err != nil {
					return err
				}
			}
			continue
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return fmt.Errorf("runner: record returned HTTP %d", response.StatusCode)
		}
		file, err := root.OpenFile(record.FileName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
		if err != nil {
			response.Body.Close()
			return err
		}
		_, err = io.Copy(file, response.Body)
		err = errors.Join(err, file.Close(), response.Body.Close())
		if err != nil {
			return err
		}
	}
	return nil
}
