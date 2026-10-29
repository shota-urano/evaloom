package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/sandboxproxy"
)

const privateHome = "/evaloom-home"

// ContainerConfig is trusted Runner wiring, not a Project Registry/API schema.
// RuntimePath must explicitly select an absolute Docker-compatible CLI. Image
// must already be installed, Linux-based, contain /usr/bin/env, and declare no
// VOLUMEs. No runtime/image/resource values are selected implicitly.
type ContainerConfig struct {
	RuntimePath string
	Image       string
	// Explicit trusted Linux helper binary and sidecar image, both prebuilt.
	// Unused for NetworkNone. The binary is mounted into sidecars only.
	ProxyImage, ProxyBinary string
	// RuntimeEnv configures the host CLI only (e.g. an explicit DOCKER_HOST).
	// It is never forwarded to the container; os.Environ is never inherited.
	RuntimeEnv map[string]string
	AllowEnv   []string
	// Trusted, dedicated roots for the corresponding mount sources. Sources
	// must resolve within these roots; a path supplied in SandboxSpec cannot
	// authorize its own mount. Roots are canonicalized once at construction.
	WorktreeRoot, GitDirRoot, ConfigDirRoot string
	// These paths must be supplied so an erroneous broad mount fails closed.
	HostHome, DataDir, RunnerTokenFile string
	// Until Q1's macOS VM option is decided, such Projects must be rejected.
	RequiresMacOS bool
	// Bounds each runtime control command, including Stop/Kill. This is not
	// the workload deadline and is also supplied explicitly by the caller.
	ControlTimeout time.Duration
	// Writers must be safe for concurrent executions and must not block.
	// Runner owns redaction/persistence; nil discards that stream.
	Stdout, Stderr io.Writer
}

type Container struct {
	config ContainerConfig
	allow  map[string]bool
	env    []string
	home   string
	data   string
	token  string
	roots  [3]string
}

var _ Sandbox = (*Container)(nil)

func NewContainer(config ContainerConfig) (*Container, error) {
	if !filepath.IsAbs(config.RuntimePath) || strings.ContainsRune(config.RuntimePath, 0) ||
		config.Image == "" || strings.HasPrefix(config.Image, "-") || strings.ContainsAny(config.Image, "\x00\r\n\t ") ||
		config.ControlTimeout <= 0 || config.RequiresMacOS {
		return nil, &Error{Op: "explicit Linux runtime, image and control timeout required"}
	}
	c := &Container{config: config, allow: make(map[string]bool)}
	var err error
	if c.home, err = resolve(config.HostHome); err != nil {
		return nil, &Error{Op: "invalid host HOME protection"}
	}
	if c.data, err = resolve(config.DataDir); err != nil {
		return nil, &Error{Op: "invalid data directory protection"}
	}
	if c.token, err = resolve(config.RunnerTokenFile); err != nil {
		return nil, &Error{Op: "invalid Runner token protection"}
	}
	for i, root := range []string{config.WorktreeRoot, config.GitDirRoot, config.ConfigDirRoot} {
		real, err := resolve(root)
		info, statErr := os.Stat(real)
		if err != nil || statErr != nil || !info.IsDir() {
			return nil, &Error{Op: "explicit mount roots must be existing absolute directories"}
		}
		c.roots[i] = real
	}
	for _, key := range config.AllowEnv {
		if !envName(key) || key == "HOME" {
			return nil, &Error{Op: "invalid environment allowlist (HOME is sandbox-owned)"}
		}
		c.allow[key] = true
	}
	for key, value := range config.RuntimeEnv {
		if !envName(key) || strings.ContainsRune(value, 0) {
			return nil, &Error{Op: "invalid runtime environment"}
		}
		c.env = append(c.env, key+"="+value)
	}
	sort.Strings(c.env)
	if c.config.Stdout == nil {
		c.config.Stdout = io.Discard
	}
	if c.config.Stderr == nil {
		c.config.Stderr = io.Discard
	}
	return c, nil
}

func envName(name string) bool {
	if name == "" {
		return false
	}
	for i, ch := range name {
		if ch != '_' && (ch < 'A' || ch > 'Z') && (ch < 'a' || ch > 'z') && (i == 0 || ch < '0' || ch > '9') {
			return false
		}
	}
	return true
}

// resolve also handles protected paths which do not exist yet, resolving their
// nearest existing parent. Mount sources themselves must exist as directories.
func resolve(path string) (string, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00,\r\n") {
		return "", os.ErrInvalid
	}
	path = filepath.Clean(path)
	real, err := filepath.EvalSymlinks(path)
	if os.IsNotExist(err) && path != filepath.Dir(path) {
		parent, parentErr := resolve(filepath.Dir(path))
		if parentErr != nil {
			return "", parentErr
		}
		return filepath.Join(parent, filepath.Base(path)), nil
	}
	return real, err
}

// Target comparisons concern the Linux container namespace, not host filesystem
// identities. Keep these lexical checks separate from all host path decisions.
func containsTarget(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func overlapsTargets(a, b string) bool { return containsTarget(a, b) || containsTarget(b, a) }

// containsOnDisk compares filesystem identities, not path spelling. In
// particular, EvalSymlinks preserves case aliases on case-insensitive macOS
// volumes. Walk existing ancestors even if a protected path does not exist yet.
// Both paths have already been resolved by resolve.
func containsOnDisk(parent, child string) (bool, error) {
	parentInfo, err := os.Stat(parent)
	if os.IsNotExist(err) {
		return false, nil // An absent parent cannot contain an existing mount.
	}
	if err != nil {
		return false, err
	}
	for {
		childInfo, err := os.Stat(child)
		if err == nil && os.SameFile(parentInfo, childInfo) {
			return true, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return false, err
		}
		next := filepath.Dir(child)
		if next == child {
			return false, nil
		}
		child = next
	}
}

func overlapsOnDisk(a, b string) (bool, error) {
	if contains, err := containsOnDisk(a, b); err != nil || contains {
		return contains, err
	}
	return containsOnDisk(b, a)
}

func (c *Container) createArgs(spec SandboxSpec, command []string, name string) ([]string, error) {
	if spec.Kind != Harness && spec.Kind != TestSetup && spec.Kind != TestRun {
		return nil, &Error{Op: "unsupported sandbox kind"}
	}
	if err := c.validateNetwork(spec); err != nil {
		return nil, err
	}
	if spec.Limits.CPU <= 0 || math.IsNaN(spec.Limits.CPU) || math.IsInf(spec.Limits.CPU, 0) ||
		spec.Limits.MemoryBytes <= 0 || spec.Limits.Time <= 0 {
		return nil, &Error{Op: "positive CPU, memory and wall-time limits required"}
	}
	if len(command) == 0 || command[0] == "" || strings.HasPrefix(command[0], "-") || strings.Contains(command[0], "=") {
		return nil, &Error{Op: "explicit command required"}
	}
	for _, arg := range command {
		if strings.ContainsRune(arg, 0) {
			return nil, &Error{Op: "invalid command argument"}
		}
	}
	if spec.Worktree == "" || (spec.Kind == Harness && (spec.GitDir == "" || spec.ConfigDir == "")) ||
		(spec.Kind != Harness && spec.ConfigDir != "") {
		return nil, &Error{Op: "missing or inappropriate sandbox directories"}
	}
	mem := strconv.FormatInt(spec.Limits.MemoryBytes, 10)
	args := []string{"container", "create", "--name", name, "--pull", "never",
		"--network", "none", "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--ipc", "private", "--cgroupns", "private",
		"--cpus", strconv.FormatFloat(spec.Limits.CPU, 'f', -1, 64), "--memory", mem, "--memory-swap", mem,
		"--init", "--no-healthcheck", "--restart", "no", "--entrypoint", "/usr/bin/env",
		"--tmpfs", privateHome + ":rw,nosuid,nodev,noexec", "--tmpfs", "/tmp:rw,nosuid,nodev",
		"--workdir", spec.Worktree}
	var sources, targets []string
	for i, path := range []string{spec.Worktree, spec.GitDir, spec.ConfigDir} {
		if path == "" {
			continue
		}
		real, err := resolve(path)
		info, statErr := os.Stat(path)
		if err != nil || statErr != nil || !info.IsDir() || path != filepath.Clean(path) || strings.ContainsAny(real, "\x00,\r\n") ||
			overlapsTargets(path, privateHome) || containsTarget(path, "/tmp") {
			return nil, &Error{Op: "unsafe mount source or target"}
		}
		withinRoot, err := containsOnDisk(c.roots[i], real)
		if err != nil || !withinRoot {
			return nil, &Error{Op: "unsafe mount source or target"}
		}
		// Dedicated managed subtrees of HOME remain allowed, but HOME itself
		// and its ancestors must never be mounted. Data/token protection is
		// bidirectional and cannot be overridden by a broad allowed root.
		for _, pair := range [][2]string{
			{real, c.home}, {real, c.data}, {c.data, real}, {real, c.token}, {c.token, real},
		} {
			protected, err := containsOnDisk(pair[0], pair[1])
			if err != nil || protected {
				return nil, &Error{Op: "unsafe mount source or target"}
			}
		}
		for j, previous := range sources {
			overlap, err := overlapsOnDisk(real, previous)
			if err != nil || overlap || overlapsTargets(path, targets[j]) {
				return nil, &Error{Op: "overlapping sandbox mounts"}
			}
		}
		sources = append(sources, real)
		targets = append(targets, path)
		mount := "type=bind,source=" + real + ",target=" + path
		if i == 2 || (i == 1 && spec.Kind != Harness) {
			mount += ",readonly"
		}
		args = append(args, "--mount", mount)
	}
	// env -i removes image ENV as well as host variables. Override the image's
	// entrypoint; command arguments are passed literally, never through a shell.
	args = append(args, c.config.Image, "-i", "HOME="+privateHome)
	var vars []string
	for key, value := range spec.Env {
		if c.allow[key] {
			if strings.ContainsRune(value, 0) {
				return nil, &Error{Op: "invalid sandbox environment"}
			}
			vars = append(vars, key+"="+value)
		}
	}
	sort.Strings(vars)
	args = append(args, vars...)
	if spec.Network == NetworkHarness || spec.Network == NetworkSetup {
		// Sandbox-owned settings override both image ENV and caller values.
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			args = append(args, key+"="+sandboxproxy.URL)
		}
		args = append(args, "NO_PROXY=", "no_proxy=")
	}
	return append(args, command...), nil
}

func (c *Container) Run(ctx context.Context, spec SandboxSpec, command []string) (Process, error) {
	name := "evaloom-" + strings.ToLower(ulid.Make().String())
	args, err := c.createArgs(spec, command, name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Limits.Time)
	fail := func(op string) (Process, error) {
		cause := ctx.Err()
		cancel()
		return nil, &Error{Op: op, Cause: cause}
	}
	var network *networkResources
	started := false
	defer func() {
		if !started && network != nil {
			network.cleanup()
		}
	}()
	// Reject implicit mounts from image VOLUMEs and non-Linux toolchains.
	image, err := c.output(ctx, "image", "inspect", "--format", "{{json .}}", c.config.Image)
	var metadata struct {
		OS     string `json:"Os"`
		Config *struct{ Volumes map[string]json.RawMessage }
	}
	if err != nil || json.Unmarshal(image, &metadata) != nil || metadata.OS != "linux" || metadata.Config == nil || len(metadata.Config.Volumes) != 0 {
		return fail("Linux image without implicit volumes required")
	}
	if spec.Network == NetworkHarness || spec.Network == NetworkSetup {
		network = &networkResources{container: c, ingress: name + "-ingress", egress: name + "-egress", volume: name + "-ipc"}
		if err := network.prepare(ctx, spec); err != nil {
			return fail("network isolation unconfirmed")
		}
		// createArgs always starts with network none; only this trusted path
		// may substitute a namespace whose complete proxy path was confirmed.
		args[slices.Index(args, "--network")+1] = "container:" + network.ingress
	}
	if _, err = c.output(ctx, args...); err != nil {
		// create cannot run a workload. Remove any partially created container
		// by its preassigned name, even if its response was lost or timed out.
		_ = c.remove(name)
		return fail("container creation failed")
	}
	if ctx.Err() != nil {
		_ = c.remove(name)
		return fail("container creation interrupted")
	}
	p := &containerProcess{container: c, name: name, done: make(chan struct{}), removed: make(chan struct{}), network: network}
	attachCtx, stopAttach := context.WithCancel(context.Background())
	attach := c.command(attachCtx, "container", "start", "--attach", name)
	attach.Stdout, attach.Stderr = c.config.Stdout, c.config.Stderr
	if err := attach.Start(); err != nil {
		stopAttach()
		_ = c.remove(name)
		return fail("container start failed")
	}
	go p.supervise(ctx, cancel, attach, stopAttach)
	started = true
	return p, nil
}
