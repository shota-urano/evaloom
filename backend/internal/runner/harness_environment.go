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

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/gitcontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
)

// PrepareHarnessEnvironment creates and verifies the dedicated configuration,
// then pins its actual files through the existing authenticated context API.
// No usable environment is returned before persistence is acknowledged. On a
// lost acknowledgement the caller may repeat with the same pins and files;
// the context API already makes that submission idempotent.
//
// The caller provides Project files and the freshly observed worktree HEAD,
// retains the returned RequestContext for HarnessRequest, and mounts ConfigDir
// in its Harness SandboxSpec with Env's variable in ContainerConfig.AllowEnv.
// For both adapters, supply this original Environment through Config.ResolveConfig;
// the adapter owns a disposable working copy and passes it to ContainerLaunch.
// HOME is supplied by the Sandbox's per-execution tmpfs, never by this method.
// After the Sandbox stops, the caller must Close the returned Environment.
func (runner *Runner) PrepareHarnessEnvironment(ctx context.Context, executionID, headSHA string,
	project []gitcontext.File, config harnessenv.Config,
) (_ *harnessenv.Environment, result adapter.RequestContext, err error) {
	if id, parseErr := ulid.ParseStrict(executionID); parseErr != nil || id.String() != executionID {
		return nil, result, fmt.Errorf("runner: invalid context execution ID")
	}
	env, err := harnessenv.Create(ctx, config)
	if err != nil {
		return nil, result, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, env.Close())
		}
	}()
	files, err := env.Files(ctx)
	if err != nil {
		return nil, result, err
	}
	if project == nil {
		project = []gitcontext.File{}
	}
	body, err := json.Marshal(struct {
		HeadSHA string            `json:"headSha"`
		Project []gitcontext.File `json:"project"`
		Harness []gitcontext.File `json:"harness"`
	}{headSHA, project, files})
	if err != nil {
		return nil, result, fmt.Errorf("runner: encode context request")
	}
	endpoint, err := url.JoinPath(runner.controlPlaneURL, "api", "executions", executionID, "context")
	if err != nil {
		return nil, result, fmt.Errorf("runner: invalid context URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, result, fmt.Errorf("runner: create context request")
	}
	request.Header.Set("Authorization", "Bearer "+runner.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := runner.client.Do(request)
	if err != nil {
		// Transport diagnostics may contain secrets. Retain only cancellation.
		return nil, result, errors.Join(fmt.Errorf("runner: context transport failed"), ctx.Err())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, result, fmt.Errorf("runner: context returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(response.Body)
	// Every submitted file and the Task input must have a revision. Global
	// Context may contribute additional entries selected by Control Plane.
	if decoder.Decode(&result) != nil || len(result.RevisionIDs) < len(project)+len(files)+1 || decoder.Decode(new(any)) != io.EOF {
		return nil, adapter.RequestContext{}, fmt.Errorf("runner: invalid context response")
	}
	return env, result, nil
}
