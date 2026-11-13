package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/harnesscontext"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
)

// Session copies are private Runner data, grouped by task workspace and
// execution. They are never mounted into another task or sent as artifacts.
func (e *executionExecutor) sessionRoot(create bool) (*os.Root, error) {
	for _, id := range []string{e.claim.Workspace.ID, e.claim.Execution.ID} {
		if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00") {
			return nil, fmt.Errorf("runner: invalid work session identity")
		}
	}
	root, err := os.OpenRoot(e.runner.dataDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	names := []string{"work-sessions", filepath.Join("work-sessions", e.claim.Workspace.ID), filepath.Join("work-sessions", e.claim.Workspace.ID, e.claim.Execution.ID)}
	for _, name := range names {
		if create {
			if err := root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return nil, err
			}
		}
		info, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("runner: session storage must not be a symlink")
		}
	}
	return root.OpenRoot(names[len(names)-1])
}

func (e *executionExecutor) restoreWorkSession(ctx context.Context, id string, original *harnessenv.Environment) (bool, error) {
	root, err := e.sessionRoot(false)
	if err != nil {
		return false, nil
	}
	defer root.Close()
	for _, name := range []string{"current", "current/configuration", "current/session-id", "current/manifest.json"} {
		info, err := root.Lstat(name)
		file := name == "current/session-id" || name == "current/manifest.json"
		if err != nil || (file && !info.Mode().IsRegular()) || (!file && !info.IsDir()) {
			return false, nil
		}
	}
	saved, err := root.ReadFile("current/session-id")
	if err != nil || string(saved) != id {
		return false, nil
	}
	data, err := root.ReadFile("current/manifest.json")
	var manifest map[string]string
	if err != nil || json.Unmarshal(data, &manifest) != nil || manifest == nil {
		return false, nil
	}
	configuration, err := root.OpenRoot("current/configuration")
	if err != nil {
		return false, nil
	}
	defer configuration.Close()
	err = e.workSession.Restore(ctx, configuration.FS(), original, manifest)
	if err != nil {
		var invalid *harnesscontext.ConfigError
		if errors.As(err, &invalid) {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func (e *executionExecutor) discardWorkSession() error {
	root, err := e.sessionRoot(false)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll("current")
}

func (e *executionExecutor) retainWorkSession(ctx context.Context, outcome StageOutcome) error {
	if outcome.RunnerError != nil || outcome.Work == nil || len(outcome.Work.Question) == 0 || outcome.Work.SessionID == nil ||
		e.workSession == nil || !e.workSession.Captured() {
		return nil
	}
	root, err := e.sessionRoot(true)
	if err != nil {
		return err
	}
	defer root.Close()
	pending := "pending-" + ulid.Make().String()
	if err := root.Mkdir(pending, 0700); err != nil {
		return err
	}
	defer root.RemoveAll(pending)
	if err := root.Mkdir(filepath.Join(pending, "configuration"), 0700); err != nil {
		return err
	}
	configuration, err := root.OpenRoot(filepath.Join(pending, "configuration"))
	if err != nil {
		return err
	}
	err = e.workSession.Save(ctx, configuration)
	closeErr := configuration.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	manifest, err := json.Marshal(e.workSession.Manifest())
	if err != nil {
		return err
	}
	metadata, err := root.OpenFile(filepath.Join(pending, "manifest.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = metadata.Write(manifest)
	closeErr = metadata.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	f, err := root.OpenFile(filepath.Join(pending, "session-id"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(*outcome.Work.SessionID)
	closeErr = f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := root.RemoveAll("current"); err != nil {
		return err
	}
	return root.Rename(pending, "current")
}
