// Package safegit runs host-side Git for the Trusted Runner and Git Publisher.
// Arguments and the executable search path belong to trusted application code;
// repository configuration, attributes and inherited GIT_* variables do not.
// Callers must stop sandbox writers before operating on their Git state. This
// wrapper disables execution settings; it does not isolate concurrent writers
// or protect refs, object contents, remote destinations or filesystem paths.
package safegit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Output runs a supported Git builtin and returns stdout. Errors retain
// *exec.ExitError (including Stderr) and context cancellation for errors.As/Is.
// No shell, user-supplied global options, aliases or external subcommands are
// accepted. Network authentication must not depend on repository helpers.
// Push requires a network transport: local receive-pack processes do not inherit
// the sender's command-line settings and could execute sandbox-written hooks.
func Output(ctx context.Context, repo string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(args) == 0 || !builtin(args[0]) {
		return nil, fmt.Errorf("safegit: unsupported command")
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	// Never place this directory in the repository or another sandbox mount.
	hooks, err := os.MkdirTemp("", "evaloom-git-hooks-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(hooks)
	env := environment(abs, args[0])
	base := []string{"--no-pager", "--no-replace-objects", "-C", abs}
	for _, setting := range fixedSettings(hooks) {
		base = append(base, "-c", setting)
	}
	config, err := configurationNames(ctx, env, base, abs)
	if err != nil {
		return nil, fmt.Errorf("safegit: read configuration: %w", err)
	}
	named, err := namedSettings(config)
	if err != nil {
		return nil, err
	}
	// Separate KEY/VALUE variables avoid -c's '=' delimiter, which can also
	// occur inside a driver name. NUL-delimited names preserve other whitespace.
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(named)))
	for i, setting := range named {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, setting.key), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, setting.value))
	}
	command := append([]string{}, args...)
	switch command[0] {
	case "ls-remote":
		// remote.*.uploadpack/receivepack use the first configuration value,
		// so a later -c cannot reset them. Command options take precedence.
		command = append([]string{command[0], "--upload-pack=git-upload-pack"}, command[1:]...)
	case "fetch", "clone":
		command = append([]string{command[0], "--upload-pack=git-upload-pack", "--no-recurse-submodules"}, command[1:]...)
	case "push":
		command = append([]string{command[0], "--receive-pack=git-receive-pack", "--recurse-submodules=no"}, command[1:]...)
	case "diff", "show", "log":
		command = append([]string{command[0], "--no-ext-diff", "--no-textconv"}, command[1:]...)
	}
	return output(ctx, env, append(base, command...)...)
}

func builtin(name string) bool {
	switch name {
	case "init", "clone", "rev-parse", "ls-tree", "cat-file", "remote", "check-ref-format",
		"ls-remote", "status", "worktree", "checkout", "switch", "restore", "add", "commit",
		"fetch", "push", "diff", "show", "log", "ls-files", "show-ref", "for-each-ref",
		"rev-list", "merge-base", "symbolic-ref":
		return true
	}
	return false
}

func fixedSettings(hooks string) []string {
	return []string{
		"core.hooksPath=" + hooks,
		"core.fsmonitor=false",
		"core.sshCommand=ssh -oBatchMode=yes", "ssh.variant=ssh",
		"core.gitProxy=", "core.askPass=/usr/bin/false",
		"credential.helper=", "credential.interactive=false",
		"core.pager=cat", "core.editor=/usr/bin/false", "sequence.editor=/usr/bin/false",
		"diff.external=/usr/bin/false",
		"interactive.diffFilter=/usr/bin/false",
		"gpg.program=/usr/bin/false", "gpg.openpgp.program=/usr/bin/false",
		"gpg.x509.program=/usr/bin/false", "gpg.ssh.program=/usr/bin/false",
		"commit.gpgSign=false", "tag.gpgSign=false", "push.gpgSign=false",
		"log.showSignature=false", "merge.verifySignatures=false",
		"core.alternateRefsCommand=/usr/bin/false",
		"submodule.recurse=false", "fetch.recurseSubmodules=false", "push.recurseSubmodules=no",
		"maintenance.auto=false", "gc.auto=0", "gc.autoDetach=false",
		"init.templateDir=" + hooks,
	}
}

type setting struct{ key, value string }

// Read includeIf targets even when their conditions are currently false:
// worktree add starts children with another gitdir and branch. Those children
// inherit our overrides, but may read a different set of includes. No command
// from configuration is executed by these config --list invocations.
func configurationNames(ctx context.Context, env, base []string, repo string) ([]byte, error) {
	var names bytes.Buffer
	visited := make(map[string]bool)
	includes := make(map[setting]bool)
	var read func(string, int) error
	read = func(file string, depth int) error {
		if depth > 10 {
			return fmt.Errorf("configuration include depth exceeded")
		}
		args := append(append([]string{}, base...), "config", "--null", "--list", "--show-origin", "--no-includes")
		if file != "" {
			if visited[file] {
				return nil
			}
			visited[file] = true
			if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
				return nil // Git ignores missing include files.
			} else if err != nil {
				return err
			}
			args = append(args, "--file", file)
		}
		data, err := output(ctx, env, args...)
		if err != nil {
			return err
		}
		records := bytes.Split(bytes.TrimSuffix(data, []byte{0}), []byte{0})
		if len(data) == 0 {
			return nil
		}
		if len(records)%2 != 0 {
			return fmt.Errorf("malformed configuration records")
		}
		for i := 0; i < len(records); i += 2 {
			key, value, _ := strings.Cut(string(records[i+1]), "\n")
			names.WriteString(key)
			names.WriteByte(0)
			lower := strings.ToLower(key)
			if value == "" || (lower != "include.path" && !(strings.HasPrefix(lower, "includeif.") && strings.HasSuffix(lower, ".path"))) {
				continue
			}
			origin, ok := strings.CutPrefix(string(records[i]), "file:")
			if !ok {
				return fmt.Errorf("include without file origin")
			}
			if !filepath.IsAbs(origin) {
				origin = repo + string(filepath.Separator) + origin
			}
			location := setting{origin, key}
			if includes[location] {
				continue
			}
			includes[location] = true
			// Let Git perform path interpolation (~user, %(prefix), etc.) rather
			// than implementing a subtly different expansion in this wrapper.
			query := append(append([]string{}, base...), "config", "--null", "--type=path", "--no-includes", "--file", origin, "--get-all", key)
			paths, err := output(ctx, env, query...)
			if err != nil {
				return err
			}
			for _, pathBytes := range bytes.Split(paths, []byte{0}) {
				if len(pathBytes) == 0 {
					continue
				}
				path := string(pathBytes)
				if !filepath.IsAbs(path) {
					// Preserve symlink/../ and the include file's original
					// spelling; cleaning or resolving changes relative includes.
					last := strings.LastIndexByte(origin, filepath.Separator)
					path = origin[:last+1] + path
				}
				if err := read(path, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := read("", 0); err != nil {
		return nil, err
	}
	return names.Bytes(), nil
}

func namedSettings(config []byte) ([]setting, error) {
	var settings []setting
	seen := make(map[string]bool)
	for _, entry := range bytes.Split(config, []byte{0}) {
		key := string(entry)
		last := strings.LastIndexByte(key, '.')
		if last < 0 {
			continue
		}
		base, leaf := key[:last], strings.ToLower(key[last+1:])
		lower := strings.ToLower(base)
		// An empty vcs value selects an empty helper rather than restoring
		// normal transport detection. Reject this selector instead of masking it.
		if strings.HasPrefix(lower, "remote.") && leaf == "vcs" {
			return nil, fmt.Errorf("safegit: external VCS transport is disabled")
		}
		if seen[base] {
			continue
		}
		switch {
		case strings.HasPrefix(lower, "hook."):
			// Newer Git also supports configuration-defined hooks, independent
			// of core.hooksPath. This disables both named hooks and event scopes.
			settings = append(settings, setting{base + ".enabled", "false"})
		case strings.HasPrefix(lower, "filter."):
			settings = append(settings, setting{base + ".clean", ""}, setting{base + ".smudge", ""}, setting{base + ".process", ""}, setting{base + ".required", "false"})
		case strings.HasPrefix(lower, "diff."):
			// Fail closed even if a trusted caller explicitly enables textconv
			// or external diffs. Do not replace content with a successful no-op.
			settings = append(settings, setting{base + ".command", "/usr/bin/false"}, setting{base + ".textconv", "/usr/bin/false"})
		case strings.HasPrefix(lower, "merge."):
			settings = append(settings, setting{base + ".driver", "/usr/bin/false"})
		case strings.HasPrefix(lower, "credential."):
			// Helpers are multi-valued and may be scoped to an arbitrary URL.
			// An empty value resets the complete helper list for that scope.
			settings = append(settings, setting{base + ".helper", ""})
		case strings.HasPrefix(lower, "remote."):
			settings = append(settings, setting{base + ".promisor", "false"})
		default:
			continue
		}
		seen[base] = true
	}
	return settings, nil
}

func environment(repo, command string) []string {
	var env []string
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if !strings.HasPrefix(name, "GIT_") && name != "SSH_ASKPASS" {
			env = append(env, variable)
		}
	}
	protocols := "file:git:http:https:ssh"
	if command == "push" {
		// Let Git enforce this after resolving remote pushurls and URL rewrites.
		// A preflight URL check could miss a second pushurl or a rewritten path.
		// Local receive-pack clears command configuration before reading its own
		// repository, so the sender's hooksPath cannot secure that child process.
		protocols = "git:http:https:ssh"
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0",
		"GIT_ASKPASS=/usr/bin/false", "SSH_ASKPASS=/usr/bin/false",
		"GIT_EDITOR=/usr/bin/false", "GIT_SEQUENCE_EDITOR=/usr/bin/false",
		"GIT_PROXY_COMMAND=",
		// Environment enforcement takes precedence over protocol.*.allow, even
		// after URL rewriting. In particular ext:: and custom helpers cannot run.
		"GIT_ALLOW_PROTOCOL="+protocols, "GIT_NO_LAZY_FETCH=1",
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(repo))
}

func output(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	// A transport/helper may hold the output pipes after Git exits. Cancel the
	// entire process group so Output obeys the caller's deadline on macOS/Unix.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, err
}
