package runner

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

var errStageOutMissing = errors.New("runner: prior stage out is missing")

// prepareStageFiles creates only a new attempt, never overwrites an earlier
// one, and keeps every out on failure/stop. Workspace lifecycle is separate.
func (runner *Runner) prepareStageFiles(id string, stage model.Stage, attempt int, request []byte, completed []model.Stage) (string, error) {
	if parsed, err := ulid.ParseStrict(id); err != nil || parsed.String() != id || !filepath.IsAbs(runner.dataDir) {
		return "", fmt.Errorf("runner: invalid stage storage identity")
	}
	if err := os.MkdirAll(runner.dataDir, 0700); err != nil {
		return "", err
	}
	data, err := os.OpenRoot(runner.dataDir)
	if err != nil {
		return "", err
	}
	defer data.Close()
	for _, name := range []string{"executions", filepath.Join("executions", id)} {
		if err := data.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if info, err := data.Lstat(name); err != nil || !info.IsDir() {
			return "", fmt.Errorf("runner: stage storage must be a directory without a symlink")
		}
	}
	execution, err := data.OpenRoot(filepath.Join("executions", id))
	if err != nil {
		return "", err
	}
	defer execution.Close()
	name := fmt.Sprintf("%s-%d", stage, attempt)
	if err := execution.Mkdir(name, 0700); err != nil {
		return "", err
	}
	root, err := execution.OpenRoot(name)
	if err != nil {
		return "", err
	}
	defer root.Close()
	for _, dir := range []string{"out", "scratch", "inputs"} {
		if err := root.Mkdir(dir, 0700); err != nil {
			return "", err
		}
	}
	file, err := root.OpenFile("request.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return "", err
	}
	_, writeErr := file.Write(request)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return "", err
	}
	for _, done := range completed {
		source, err := latestStageOut(execution, done)
		if err != nil {
			return "", err
		}
		err = copyStageInput(source, root, filepath.Join("inputs", string(done)))
		closeErr := source.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return "", err
		}
	}
	if err := root.Chmod("inputs", 0500); err != nil {
		return "", err
	}
	return filepath.Join(runner.dataDir, "executions", id, name), nil
}

// completedStages names stages, not attempts. Their most recent attempt is
// the saved out to reuse, including a check paused for approval or judgment.
// A missing latest out must not silently fall back to stale, older content.
func latestStageOut(execution *os.Root, stage model.Stage) (*os.Root, error) {
	entries, err := fs.ReadDir(execution.FS(), ".")
	if err != nil {
		return nil, err
	}
	latest := 0
	var name string
	for _, entry := range entries {
		suffix, ok := strings.CutPrefix(entry.Name(), string(stage)+"-")
		if !ok {
			continue
		}
		attempt, err := strconv.Atoi(suffix)
		if err == nil && strconv.Itoa(attempt) == suffix && attempt > latest {
			latest, name = attempt, entry.Name()
		}
	}
	if name == "" {
		return nil, errStageOutMissing
	}
	for _, path := range []string{name, filepath.Join(name, "out")} {
		info, err := execution.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, errStageOutMissing
		}
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("runner: prior stage out must be a directory without a symlink")
		}
	}
	return execution.OpenRoot(filepath.Join(name, "out"))
}

func copyStageInput(source, destination *os.Root, target string) error {
	// os.Root confines reads and writes; rejecting symlinks also prevents a
	// stage from forwarding files outside its out as inputs to a later stage.
	err := fs.WalkDir(source.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		to := filepath.Join(target, name)
		if entry.IsDir() {
			return destination.Mkdir(to, 0700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("runner: stage input must be a regular file")
		}
		input, err := source.Open(name)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := destination.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
		if err != nil {
			return err
		}
		_, err = io.Copy(output, input)
		return errors.Join(err, output.Close())
	})
	if err != nil {
		return err
	}
	return fs.WalkDir(source.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return destination.Chmod(filepath.Join(target, name), 0500)
		}
		return nil
	})
}
