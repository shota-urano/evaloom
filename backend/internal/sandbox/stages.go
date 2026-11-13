package sandbox

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/sandboxproxy"
)

// Stage paths are caller-supplied: the paths in contract 6.1 are examples,
// not reserved constants. Keep the authorization roots outside SandboxSpec.
func (c *Container) configureStageMounts() error {
	if len(c.config.StageMountRoots) == 0 {
		return nil // Existing Adapter wiring may still use Harness.
	}
	p := c.config.StagePaths
	paths := []string{p.Profile, p.Workspace, p.Out, p.Inputs, p.Records, p.Scratch}
	if len(c.config.StageMountRoots) != len(paths) {
		return &Error{Op: "six dedicated stage mount roots required"}
	}
	c.stageRoots = make(map[string]string)
	for i, target := range paths {
		if !stageTarget(target) {
			return &Error{Op: "invalid stage request path"}
		}
		root, err := resolve(c.config.StageMountRoots[target])
		info, statErr := os.Stat(root)
		if err != nil || statErr != nil || !info.IsDir() || !c.safeStageSource(root) {
			return &Error{Op: "unsafe stage mount root"}
		}
		for j := 0; j < i; j++ {
			overlap, err := overlapsOnDisk(root, c.stageRoots[paths[j]])
			if err != nil || overlap || overlapsTargets(target, paths[j]) {
				return &Error{Op: "overlapping stage mount roots or paths"}
			}
		}
		c.stageRoots[target] = root
	}
	return nil
}

func stageTarget(target string) bool {
	return filepath.IsAbs(target) && filepath.Clean(target) == target &&
		!strings.ContainsAny(target, "\x00,\r\n:") &&
		!overlapsTargets(target, "/tmp") && !overlapsTargets(target, privateHome) &&
		!overlapsTargets(target, "/proc") && !overlapsTargets(target, "/sys") &&
		!overlapsTargets(target, "/dev")
}

func (c *Container) safeStageSource(source string) bool {
	// Runner stores extracted profiles and per-attempt files under dataDir.
	// Only explicitly authorized, disjoint subtrees may be shared, never the
	// data directory itself/its ancestors or the Runner token (in either direction).
	for _, pair := range [][2]string{{source, c.home}, {source, c.data}, {source, c.token}, {c.token, source}} {
		contains, err := containsOnDisk(pair[0], pair[1])
		if err != nil || contains {
			return false
		}
	}
	keys, err := resolve(filepath.Join(c.data, "secrets"))
	if err != nil {
		return false
	}
	if overlap, err := overlapsOnDisk(source, keys); err != nil || overlap {
		return false
	}
	return true
}

func (c *Container) stageArgs(spec SandboxSpec, command []string, name string, configVolume ...string) ([]string, error) {
	p := c.config.StagePaths
	// Values are the exact writable permissions in contract 6.2/spec 09 3.1.
	permissions := map[string]bool{p.Out: true, p.Scratch: true}
	workdir := p.Profile
	switch spec.Kind {
	case Prepare:
		permissions[p.Profile], permissions[p.Workspace] = false, true
	case Work:
		permissions[p.Workspace], permissions[p.Inputs], permissions[p.Records] = true, false, false
		workdir = p.Workspace
	case Check, Publish:
		permissions[p.Profile], permissions[p.Workspace], permissions[p.Inputs] = false, false, false
	case Proposal:
		permissions[p.Inputs] = false
		// The proposal working directory is not defined by spec 15. Do not
		// select one here; its command/image wiring remains the caller's boundary.
		workdir = ""
	default:
		return nil, &Error{Op: "unsupported sandbox kind"}
	}
	if len(c.stageRoots) == 0 || spec.Worktree != "" || spec.GitDir != "" {
		return nil, &Error{Op: "explicit stage mount wiring required"}
	}
	channelMount, err := c.channelMount(spec)
	if err != nil {
		return nil, err
	}
	if (spec.ConfigDir != "" || spec.ConfigWritable) && spec.Kind != Work && spec.Kind != Proposal {
		return nil, &Error{Op: "configuration is only available to work and proposal"}
	}
	if spec.ConfigWritable && spec.ConfigDir == "" {
		return nil, &Error{Op: "configuration directory required"}
	}
	home := filepath.Join(p.Scratch, ".home-"+name)
	mem := strconv.FormatInt(spec.Limits.MemoryBytes, 10)
	args := []string{"container", "create", "--name", name, "--pull", "never",
		"--network", "none", "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--ipc", "private", "--cgroupns", "private",
		"--cpus", strconv.FormatFloat(spec.Limits.CPU, 'f', -1, 64), "--memory", mem, "--memory-swap", mem,
		"--init", "--no-healthcheck", "--restart", "no", "--entrypoint", "/usr/bin/env",
		"--tmpfs", home + ":rw,nosuid,nodev,noexec", "--tmpfs", "/tmp:rw,nosuid,nodev"}
	if workdir != "" {
		args = append(args, "--workdir", workdir)
	}
	seen := map[string]bool{}
	var sources []string
	for _, mount := range spec.Mounts {
		writable, allowed := permissions[mount.GuestPath]
		if !allowed || seen[mount.GuestPath] || mount.Writable != writable {
			return nil, &Error{Op: "mount violates stage visibility or write permissions"}
		}
		real, err := c.stageSource(mount.HostPath, c.stageRoots[mount.GuestPath], sources)
		if err != nil {
			return nil, err
		}
		sources = append(sources, real)
		seen[mount.GuestPath] = true
		arg := "type=bind,source=" + real + ",target=" + mount.GuestPath + ",bind-recursive=disabled"
		if !mount.Writable {
			arg += ",readonly"
		}
		args = append(args, "--mount", arg)
	}
	if len(seen) != len(permissions) {
		return nil, &Error{Op: "missing stage mount"}
	}
	if spec.ConfigDir != "" {
		if !stageTarget(spec.ConfigDir) || c.roots[2] == "" {
			return nil, &Error{Op: "explicit configuration root required"}
		}
		for target := range c.stageRoots {
			if overlapsTargets(spec.ConfigDir, target) {
				return nil, &Error{Op: "configuration overlaps stage path"}
			}
		}
		// Config must not alias a hidden profile (or any other stage region),
		// even when that region is absent from this stage's actual mounts.
		var regions []string
		for _, root := range c.stageRoots {
			regions = append(regions, root)
		}
		real, err := c.stageSource(spec.ConfigDir, c.roots[2], regions)
		if err != nil {
			return nil, err
		}
		mount := "type=bind,source=" + real + ",target=" + spec.ConfigDir + ",bind-recursive=disabled,readonly"
		if spec.ConfigWritable {
			if len(configVolume) != 1 || configVolume[0] == "" {
				return nil, &Error{Op: "verified configuration capability required"}
			}
			mount = "type=volume,source=" + configVolume[0] + ",target=" + spec.ConfigDir + ",volume-nocopy"
		}
		args = append(args, "--mount", mount)
	}
	if channelMount != "" {
		args = append(args, "--mount", channelMount)
	}
	if c.config.StageRequestFile != "" {
		source, err := resolve(c.config.StageRequestFile)
		info, statErr := os.Lstat(c.config.StageRequestFile)
		target := c.config.StageRequestPath
		if err != nil || statErr != nil || !info.Mode().IsRegular() || !c.safeStageSource(source) ||
			!stageTarget(target) || spec.Env["EVALOOM_REQUEST"] != target {
			return nil, &Error{Op: "invalid stage request mount"}
		}
		for guest := range c.stageRoots {
			if overlapsTargets(target, guest) {
				return nil, &Error{Op: "request overlaps stage region"}
			}
		}
		if spec.ConfigDir != "" && overlapsTargets(target, spec.ConfigDir) {
			return nil, &Error{Op: "request overlaps configuration"}
		}
		args = append(args, "--mount", "type=bind,source="+source+",target="+target+",readonly")
	}
	// env -i strips host and image ENV. The contract allowlist is enforced
	// even if the trusted legacy AllowEnv contains additional variable names.
	secrets, err := stageSecretEnvironment(spec)
	if err != nil {
		return nil, err
	}
	for _, entry := range secrets {
		key, _, _ := strings.Cut(entry, "=")
		args = append(args, "--env", key)
	}
	args = append(args, c.config.Image, "-i", "HOME="+home, "TZ=UTC")
	var env []string
	for key, value := range spec.Env {
		if !stageEnvAllowed(spec.Kind, key) {
			continue
		}
		if key == "EVALOOM_CHANNEL" && spec.ChannelPath == "" {
			continue // Never advertise an absent or caller-injected channel.
		}
		if strings.ContainsRune(value, 0) {
			return nil, &Error{Op: "invalid sandbox environment"}
		}
		if key == "CODEX_HOME" || key == "CLAUDE_CONFIG_DIR" {
			if spec.ConfigDir == "" || value != spec.ConfigDir {
				return nil, &Error{Op: "configuration environment must reference dedicated configuration"}
			}
		}
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	if len(spec.AllowedDestinations) != 0 {
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			env = append(env, key+"="+sandboxproxy.URL)
		}
		env = append(env, "NO_PROXY=", "no_proxy=")
	}
	return append(append(args, env...), command...), nil
}

func stageEnvAllowed(kind SandboxKind, key string) bool {
	switch key {
	case "PATH", "LANG":
		return true
	case "EVALOOM_REQUEST":
		return kind != Proposal
	case "EVALOOM_RESULT", "EVALOOM_CHANNEL":
		return kind == Prepare || kind == Check || kind == Publish
	case "CODEX_HOME", "CLAUDE_CONFIG_DIR":
		return kind == Work || kind == Proposal
	default:
		return false // HOME and TZ are owned by the sandbox.
	}
}

func (c *Container) stageSource(source, root string, previous []string) (string, error) {
	real, err := resolve(source)
	info, statErr := os.Stat(real)
	if err != nil || statErr != nil || !info.IsDir() || !c.safeStageSource(real) {
		return "", &Error{Op: "unsafe stage mount source"}
	}
	within, err := containsOnDisk(root, real)
	if err != nil || !within {
		return "", &Error{Op: "stage mount source outside dedicated root"}
	}
	for _, other := range previous {
		overlap, err := overlapsOnDisk(real, other)
		if err != nil || overlap {
			return "", &Error{Op: "overlapping stage mount sources"}
		}
	}
	return real, nil
}
