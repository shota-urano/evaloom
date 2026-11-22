package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/sandboxproxy"
)

// ChannelHandler is the Runner's stage channel (contract 7.4, spec 05 3.7).
// HandleChannelRequest receives one request line without its LF and returns
// one reply line without its LF, or nil to close that connection without a
// reply. This is internal Runner wiring, not an API or configuration key.
type ChannelHandler interface {
	HandleChannelRequest(line []byte) []byte
}

// RelayChannel connects a relay sidecar's stdout (requests) and stdin
// (replies) to handler. Requests are handled as they arrive, so concurrent
// stage requests are not serialized behind a slow authorization, but replies
// are written in request order: that order is how the sidecar matches them.
// It returns when the sidecar closes its output or sends anything other than
// one line of bounded JSON, after every started request has been answered;
// the caller then closes stdin, which stops the sidecar.
func RelayChannel(stdout io.Reader, stdin io.Writer, handler ChannelHandler) {
	replies := make(chan chan []byte, 64)
	written := make(chan struct{})
	go func() {
		defer close(written)
		failed := false
		for slot := range replies {
			reply := <-slot
			if failed {
				continue
			}
			if reply == nil {
				reply = sandboxproxy.ChannelReject
			}
			if _, err := stdin.Write(append(append([]byte(nil), reply...), '\n')); err != nil {
				failed = true
			}
		}
	}()
	defer func() {
		close(replies)
		<-written
	}()
	reader := sandboxproxy.NewChannelReader(stdout)
	for {
		line, err := sandboxproxy.ReadChannelLine(reader)
		if err != nil || !sandboxproxy.ValidChannelLine(line) {
			return
		}
		slot := make(chan []byte, 1)
		replies <- slot
		go func(line []byte) { slot <- handler.HandleChannelRequest(line) }(append([]byte(nil), line...))
	}
}

// channelTarget returns the guest directory for the stage's channel volume.
// EVALOOM_CHANNEL names the socket inside it; the sidecar's socket name is fixed.
func (c *Container) channelTarget(spec SandboxSpec) (string, error) {
	if spec.Channel == nil {
		return "", nil
	}
	if spec.Kind != Prepare && spec.Kind != Check && spec.Kind != Publish {
		return "", &Error{Op: "channel is unavailable for this sandbox kind"}
	}
	socket := spec.Env["EVALOOM_CHANNEL"]
	target := filepath.Dir(socket)
	if !stageTarget(socket) || filepath.Base(socket) != sandboxproxy.ChannelName || !stageTarget(target) || target == "/" {
		return "", &Error{Op: "explicit channel guest path required"}
	}
	for guest := range c.stageRoots {
		if overlapsTargets(target, guest) {
			return "", &Error{Op: "channel overlaps stage region"}
		}
	}
	if spec.ConfigDir != "" && overlapsTargets(target, spec.ConfigDir) {
		return "", &Error{Op: "channel overlaps configuration"}
	}
	if c.config.StageRequestPath != "" && overlapsTargets(target, c.config.StageRequestPath) {
		return "", &Error{Op: "channel overlaps request"}
	}
	if err := c.validateChannelRelay(); err != nil {
		return "", err
	}
	return target, nil
}

// channelResources is one stage's channel: a private volume and a relay
// sidecar (spec 09 3.6, Q232). No host socket is shared with any container.
type channelResources struct {
	container    *Container
	name, volume string
	handler      ChannelHandler
	attach       *exec.Cmd
	stopAttach   context.CancelFunc
	stdin        io.WriteCloser
	relayed      chan struct{}
	cleanOnce    sync.Once
}

func channelVolume(name string) string { return name + "-channel" }

func (c *Container) validateChannelRelay() error {
	if c.config.ProxyImage == "" || strings.HasPrefix(c.config.ProxyImage, "-") || strings.ContainsAny(c.config.ProxyImage, "\x00\r\n\t ") {
		return &Error{Op: "channel relay image required"}
	}
	if !c.trustedProxyBinary() {
		return &Error{Op: "channel relay binary required"}
	}
	return nil
}

func (r *channelResources) prepare(ctx context.Context, spec SandboxSpec) error {
	c := r.container
	image, err := c.output(ctx, "image", "inspect", "--format", "{{json .}}", c.config.ProxyImage)
	var metadata struct {
		OS     string `json:"Os"`
		Config *struct{ Volumes map[string]json.RawMessage }
	}
	if err != nil {
		return err
	}
	if json.Unmarshal(image, &metadata) != nil || metadata.OS != "linux" || metadata.Config == nil || len(metadata.Config.Volumes) != 0 {
		return runtimeDetail("channel relay image is not a Linux image without volumes")
	}
	if _, err := c.output(ctx, "volume", "create", "--driver", "local", r.volume); err != nil {
		return err
	}
	binary, err := resolve(c.config.ProxyBinary)
	if err != nil {
		return runtimeDetail("channel relay binary unresolved")
	}
	mem := strconv.FormatInt(spec.Limits.MemoryBytes, 10)
	// The sidecar gets no environment, no network and no credentials: only the
	// trusted relay binary and the stage's private channel volume.
	args := []string{"container", "create", "--name", r.name, "--pull", "never", "--interactive",
		"--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--ipc", "private", "--cgroupns", "private", "--init", "--restart", "no", "--no-healthcheck",
		"--cpus", strconv.FormatFloat(spec.Limits.CPU, 'f', -1, 64), "--memory", mem, "--memory-swap", mem,
		"--mount", "type=bind,source=" + binary + ",target=" + proxyExecutable + ",readonly",
		"--mount", "type=volume,source=" + r.volume + ",target=" + sandboxproxy.ChannelDirectory + ",volume-nocopy",
		"--entrypoint", proxyExecutable, c.config.ProxyImage, "channel", (spec.Limits.Time + TerminationGrace).String()}
	if _, err := c.output(ctx, args...); err != nil {
		return err
	}
	attachCtx, stopAttach := context.WithCancel(context.Background())
	attach := c.command(attachCtx, "container", "start", "--attach", "--interactive", r.name)
	stdin, err := attach.StdinPipe()
	if err != nil {
		stopAttach()
		return runtimeDetail("channel relay stdio unavailable")
	}
	stdout, err := attach.StdoutPipe()
	if err != nil {
		stopAttach()
		return runtimeDetail("channel relay stdio unavailable")
	}
	attach.Stderr = io.Discard
	if err := attach.Start(); err != nil {
		stopAttach()
		return runtimeFailure(err)
	}
	r.attach, r.stopAttach, r.stdin, r.relayed = attach, stopAttach, stdin, make(chan struct{})
	go func() {
		defer close(r.relayed)
		RelayChannel(stdout, stdin, r.handler)
		_ = stdin.Close()
	}()
	// The stage starts only after the sidecar listens in the volume. The
	// attach client starts the sidecar asynchronously, so retry until the
	// control deadline unless the sidecar has already gone.
	deadline := time.Now().Add(c.config.ControlTimeout)
	for {
		_, err = c.output(ctx, "container", "exec", r.name, proxyExecutable, "channel-check", (c.config.ControlTimeout / 2).String())
		if err == nil {
			return nil
		}
		select {
		case <-r.relayed:
			return err
		case <-ctx.Done():
			return err
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return err
		}
	}
}

// cleanup removes the sidecar, then the volume. The stage container has
// already been removed, so no stage process can still hold the socket. A
// failed removal is logged and retried, as for the network sidecars.
func (r *channelResources) cleanup() {
	r.cleanOnce.Do(func() {
		c := r.container
		for attempt := 0; c.remove(r.name) != nil; attempt++ {
			if attempt == 0 {
				_, _ = io.WriteString(c.config.Stderr, "evaloom: stage channel relay removal failed; retrying\n")
			}
			time.Sleep(c.config.ControlTimeout)
		}
		if r.attach != nil {
			_ = r.stdin.Close()
			r.stopAttach()
			<-r.relayed
			_ = r.attach.Wait()
		}
		for attempt := 0; ; attempt++ {
			if _, err := c.output(context.Background(), "volume", "rm", r.volume); err == nil {
				return
			}
			out, err := c.output(context.Background(), "volume", "ls", "--filter", "name=^"+r.volume+"$", "--format", "{{.Name}}")
			if err == nil && strings.TrimSpace(string(out)) == "" {
				return
			}
			if attempt == 0 {
				_, _ = io.WriteString(c.config.Stderr, "evaloom: stage channel volume removal failed; retrying\n")
			}
			time.Sleep(c.config.ControlTimeout)
		}
	})
}
