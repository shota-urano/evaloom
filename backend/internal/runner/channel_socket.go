package runner

import (
	"net"
	"os"
	"path/filepath"
	"sync"
)

// Bind through a short address without moving the socket out of the attempt
// directory. Advertise the absolute host path for Sandbox mounts and capability
// checks; the kernel's short bind address is not usable after binding returns.
type channelListener struct {
	*net.UnixListener
	path     string
	identity os.FileInfo
	unlink   sync.Once
}

func listenStageChannel(path string) (*channelListener, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	var listener *net.UnixListener
	err = withChannelAddress(path, func(address string) error {
		var err error
		listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
		if err == nil {
			listener.SetUnlinkOnClose(false)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		listener.Close()
		return nil, err
	}
	return &channelListener{UnixListener: listener, path: path, identity: info}, nil
}

func (l *channelListener) Addr() net.Addr {
	return &net.UnixAddr{Name: l.path, Net: "unix"}
}

func (l *channelListener) Close() error {
	err := l.UnixListener.Close()
	l.unlink.Do(func() {
		// The bind address was relative or used a directory fd. Unlink by the
		// retained absolute path, and never remove a replacement file.
		if info, err := os.Lstat(l.path); err == nil && os.SameFile(l.identity, info) {
			_ = os.Remove(l.path)
		}
	})
	return err
}
