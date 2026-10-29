package sandbox

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/sandboxproxy"
)

const proxyExecutable = "/evaloom-proxy"

func (c *Container) validateNetwork(spec SandboxSpec) error {
	deny := func() error { return &Error{Op: "invalid network policy or explicit proxy configuration"} }
	switch spec.Network {
	case "", NetworkNone:
		if len(spec.AllowedDestinations) != 0 {
			return deny()
		}
		return nil
	case NetworkHarness:
		if spec.Kind != Harness {
			return deny()
		}
	case NetworkSetup:
		if spec.Kind != TestSetup {
			return deny()
		}
	default:
		return deny()
	}
	if len(spec.AllowedDestinations) == 0 || c.config.ProxyImage == "" ||
		strings.HasPrefix(c.config.ProxyImage, "-") || strings.ContainsAny(c.config.ProxyImage, "\x00\r\n\t ") {
		return deny()
	}
	path, err := resolve(c.config.ProxyBinary)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return deny()
	}
	// Neither a workload nor an earlier harness may replace the trusted helper.
	for _, root := range c.roots {
		inside, err := containsOnDisk(root, path)
		if err != nil || inside {
			return deny()
		}
	}
	for _, destination := range spec.AllowedDestinations {
		if _, err := sandboxproxy.Destination(destination); err != nil {
			return deny()
		}
		host, _, _ := net.SplitHostPort(destination)
		if spec.Kind == Harness && (host == "github.com" || strings.HasSuffix(host, ".github.com")) {
			return deny()
		}
	}
	return nil
}

type networkResources struct {
	container               *Container
	ingress, egress, volume string
}

func (n *networkResources) prepare(ctx context.Context, spec SandboxSpec) error {
	c := n.container
	image, err := c.output(ctx, "image", "inspect", "--format", "{{json .}}", c.config.ProxyImage)
	var metadata struct {
		OS     string `json:"Os"`
		Config *struct{ Volumes map[string]json.RawMessage }
	}
	if err != nil || json.Unmarshal(image, &metadata) != nil || metadata.OS != "linux" || metadata.Config == nil || len(metadata.Config.Volumes) != 0 {
		return &Error{Op: "proxy image unconfirmed"}
	}
	if _, err := c.output(ctx, "volume", "create", "--driver", "local", n.volume); err != nil {
		return err
	}
	binary, err := resolve(c.config.ProxyBinary)
	if err != nil {
		return err
	}
	mem := strconv.FormatInt(spec.Limits.MemoryBytes, 10)
	for _, helper := range []struct{ name, network, mode string }{
		{n.egress, "bridge", "egress"}, {n.ingress, "none", "ingress"},
	} {
		args := []string{"container", "create", "--name", helper.name, "--pull", "never",
			"--network", helper.network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
			"--ipc", "private", "--cgroupns", "private", "--init", "--restart", "no", "--no-healthcheck",
			"--cpus", strconv.FormatFloat(spec.Limits.CPU, 'f', -1, 64), "--memory", mem, "--memory-swap", mem,
			"--mount", "type=bind,source=" + binary + ",target=" + proxyExecutable + ",readonly",
			"--mount", "type=volume,source=" + n.volume + ",target=/evaloom-ipc,volume-nocopy",
			"--entrypoint", proxyExecutable, c.config.ProxyImage, helper.mode, spec.Limits.Time.String()}
		if helper.mode == "egress" {
			args = append(args, spec.AllowedDestinations...)
		}
		if _, err := c.output(ctx, args...); err != nil {
			return err
		}
		if _, err := c.output(ctx, "container", "start", helper.name); err != nil {
			return err
		}
		out, err := c.output(ctx, "container", "inspect", "--format", "{{json .}}", helper.name)
		var inspect struct {
			State           struct{ Running bool }
			HostConfig      struct{ NetworkMode string }
			NetworkSettings struct{ Networks map[string]json.RawMessage }
		}
		if err != nil || json.Unmarshal(out, &inspect) != nil || !inspect.State.Running ||
			inspect.HostConfig.NetworkMode != helper.network || len(inspect.NetworkSettings.Networks) != 1 || inspect.NetworkSettings.Networks[helper.network] == nil {
			return &Error{Op: "proxy network namespace unconfirmed"}
		}
	}
	// Confirm TCP ingress -> private Unix volume -> filtering egress. The
	// check retries startup races and succeeds only on an explicit denial.
	_, err = c.output(ctx, "container", "exec", n.ingress, proxyExecutable, "check", (c.config.ControlTimeout / 2).String())
	return err
}

func (n *networkResources) cleanup() {
	// Stop namespace users before removing the shared socket. Wait does not
	// release capacity while a trusted sidecar's removal is unconfirmed.
	for _, name := range []string{n.ingress, n.egress} {
		for n.container.remove(name) != nil {
			time.Sleep(n.container.config.ControlTimeout)
		}
	}
	for {
		if _, err := n.container.output(context.Background(), "volume", "rm", n.volume); err == nil {
			return
		}
		out, err := n.container.output(context.Background(), "volume", "ls", "--filter", "name="+n.volume, "--format", "{{.Name}}")
		if err == nil && strings.TrimSpace(string(out)) == "" {
			return
		}
		time.Sleep(n.container.config.ControlTimeout)
	}
}
