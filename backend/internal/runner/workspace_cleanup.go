package runner

import (
	"fmt"
	"os"
	"strings"
)

// cleanWorkspace only accepts a direct child of workspaceRoot (spec 05 3.5).
// os.Root pins the directory and prevents nested symlinks or concurrent renames
// from redirecting recursive deletion outside it. Do not follow a workspace
// symlink, even when its target is another workspace inside the root.
func (runner *Runner) cleanWorkspace(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00") {
		return fmt.Errorf("runner: invalid cleanup workspace ID")
	}
	root, err := os.OpenRoot(runner.workspaceRoot)
	if os.IsNotExist(err) {
		// A repeated instruction (including after a restart) is already done.
		return nil
	}
	if err != nil {
		return fmt.Errorf("runner: cannot open workspaceRoot for cleanup")
	}
	defer root.Close()
	info, err := root.Lstat(id)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("runner: cannot inspect cleanup workspace")
	}
	if !info.IsDir() {
		return fmt.Errorf("runner: cleanup workspace must be a directory without a symlink")
	}
	if err := root.RemoveAll(id); err != nil {
		return fmt.Errorf("runner: cannot remove cleanup workspace")
	}
	return nil
}
