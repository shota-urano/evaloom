package runner

import (
	"fmt"
	"os"
	"path/filepath"
)

func withChannelAddress(path string, use func(string) error) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	// procfs resolves this directory fd to the original attempt directory;
	// the resulting socket inode stays there, with no process-wide chdir.
	address := fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(path))
	return use(address)
}
