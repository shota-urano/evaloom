package sandbox

import (
	"context"
	"net"
	"os"
	"path/filepath"
)

type channelKey struct{}

type stageChannelBinding struct {
	kind     SandboxKind
	listener *net.UnixListener
	path     string
	identity os.FileInfo
}

// WithStageChannel grants a single Runner-created listener to one stage. A
// SandboxSpec path alone cannot grant access to arbitrary host/control sockets.
// The caller keeps the listener alive until every stage process has stopped.
// This is internal Runner wiring, not an API or a Runner configuration key.
func WithStageChannel(ctx context.Context, kind SandboxKind, listener *net.UnixListener) context.Context {
	if listener == nil {
		return ctx
	}
	path := listener.Addr().String()
	info, _ := os.Lstat(path)
	return context.WithValue(ctx, channelKey{}, stageChannelBinding{kind, listener, path, info})
}

func validateStageChannel(ctx context.Context, spec SandboxSpec) error {
	if spec.ChannelPath == "" {
		return nil
	}
	binding, ok := ctx.Value(channelKey{}).(stageChannelBinding)
	info, err := os.Lstat(spec.ChannelPath)
	if !ok || binding.kind != spec.Kind || binding.path != spec.ChannelPath ||
		binding.identity == nil || err != nil || info.Mode()&os.ModeSocket == 0 ||
		!os.SameFile(binding.identity, info) {
		return &Error{Op: "verified stage channel required"}
	}
	// SyscallConn fails after Close, even if an attacker recreated the pathname.
	raw, err := binding.listener.SyscallConn()
	if err != nil {
		return &Error{Op: "stage channel is closed"}
	}
	if err := raw.Control(func(uintptr) {}); err != nil {
		return &Error{Op: "stage channel is closed"}
	}
	return nil
}

func (c *Container) channelMount(spec SandboxSpec) (string, error) {
	if spec.ChannelPath == "" {
		return "", nil
	}
	if spec.Kind != Prepare && spec.Kind != Check && spec.Kind != Publish {
		return "", &Error{Op: "channel is unavailable for this sandbox kind"}
	}
	target := spec.Env["EVALOOM_CHANNEL"]
	if !stageTarget(target) || !filepath.IsAbs(spec.ChannelPath) ||
		filepath.Clean(spec.ChannelPath) != spec.ChannelPath {
		return "", &Error{Op: "explicit channel source and guest path required"}
	}
	real, err := resolve(spec.ChannelPath)
	info, statErr := os.Lstat(spec.ChannelPath)
	if err != nil || statErr != nil || info.Mode()&os.ModeSocket == 0 || !c.safeStageSource(real) {
		return "", &Error{Op: "unsafe stage channel source"}
	}
	for guest, root := range c.stageRoots {
		overlap, err := overlapsOnDisk(real, root)
		if err != nil || overlap || overlapsTargets(target, guest) {
			return "", &Error{Op: "channel overlaps stage region"}
		}
	}
	if spec.ConfigDir != "" && overlapsTargets(target, spec.ConfigDir) {
		return "", &Error{Op: "channel overlaps configuration"}
	}
	// Share only the socket inode, never its parent (which also holds the
	// request and other stage-private files). Read-only bind still permits IPC.
	return "type=bind,source=" + real + ",target=" + target + ",readonly", nil
}
