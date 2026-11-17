package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unicode/utf8"

	"github.com/shota-urano/evaloom/backend/internal/contract"
)

const maxWorkOutput = 64 << 10 // Spec 08 Q144, in bytes.

// Only a successful CLI may ask a question or supply a work summary. Other
// out files remain opaque artifacts; none can supply a check-stage verdict.
func (e *executionExecutor) collectWorkOutputs(ctx context.Context, in StageInvocation, outcome *StageOutcome) error {
	if outcome.RunnerError != nil || outcome.ExitCode == nil || *outcome.ExitCode != 0 || outcome.Work == nil {
		return nil
	}
	root, err := os.OpenRoot(in.Directory)
	if err != nil {
		return err
	}
	defer root.Close()
	if info, err := root.Lstat("out"); err != nil || !info.IsDir() {
		return fmt.Errorf("runner: unsafe work output directory")
	}
	out, err := root.OpenRoot("out")
	if err != nil {
		return err
	}
	defer out.Close()
	question, err := readWorkOutput(out, "question.json")
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		outcome.RunnerError = strPtr("work_failed")
	default:
		// Preserve even malformed/oversized regular questions as artifacts. The
		// bounded read above is for interpretation; archival never truncates it.
		file, err := out.OpenFile("question.json", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		artifact, collectErr := in.CollectArtifact("question", "question.json", file)
		if err := errors.Join(collectErr, file.Close()); err != nil {
			return err
		}
		if err := e.runner.deliverArtifact(ctx, artifact); err != nil {
			return err
		}
		if len(question) > maxWorkOutput || contract.ValidateWorkQuestion(question) != nil {
			outcome.RunnerError = strPtr("work_failed")
		} else {
			outcome.Work.Question = in.redactBytes(question)
		}
	}
	summary, err := readWorkOutput(out, "summary.md")
	if err != nil {
		// An optional summary is read only when it is a regular file. In
		// particular a symlink never becomes an artifact or affects success.
		return nil
	}
	truncated := len(summary) > maxWorkOutput
	if truncated {
		// Exclude before truncating, so a credential crossing the display
		// boundary cannot leave its unredacted prefix in the saved summary.
		// Retained out files also pass through this full-file exclusion.
		summary, err = out.ReadFile("summary.md")
		if err != nil {
			return err
		}
	}
	summary = in.redactBytes(summary)
	if len(summary) > maxWorkOutput {
		truncated = true
		// Do not leave a partial UTF-8 character at the truncation boundary.
		end := maxWorkOutput
		for end > maxWorkOutput-utf8.UTFMax && !utf8.RuneStart(summary[end]) {
			end--
		}
		summary = summary[:end]
	}
	if truncated {
		summary = append(summary, []byte("\n\n[Truncated: summary.md exceeded 64 KiB]\n")...)
	}
	artifact, err := e.collect(ctx, in, "work_summary", "summary.md", summary)
	if err == nil {
		outcome.Work.SummaryArtifactID = &artifact.ID
	}
	return err
}

func readWorkOutput(root *os.Root, name string) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("runner: work output is not a regular file")
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, maxWorkOutput+1))
}
