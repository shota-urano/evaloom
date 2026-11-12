package runner

import (
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

func withChannelAddress(path string, use func(string) error) error {
	done := make(chan error)
	go func() {
		// A thread-local cwd leaves concurrent stages and all other relative
		// filesystem operations unaffected. Exit while locked so Go retires
		// this OS thread instead of reusing it with the stage's cwd.
		runtime.LockOSThread()
		if err := unix.PthreadChdir(filepath.Dir(path)); err != nil {
			done <- err
			return
		}
		done <- use(filepath.Base(path))
	}()
	return <-done
}
