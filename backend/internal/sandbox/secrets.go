package sandbox

import (
	"context"
	"sort"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

// SandboxSpec contains already resolved, declared slots. The Runner's existing
// WithStageSecrets/Store.ForStage boundary checks declarations and stored access
// before Run. Sandbox independently rejects keys in all other kinds.
func stageSecretEnvironment(spec SandboxSpec) ([]string, error) {
	if len(spec.Secrets) != 0 && spec.Kind != Prepare && spec.Kind != Publish {
		return nil, &Error{Op: "secrets are only available to prepare and publish"}
	}
	var env []string
	for slot, value := range spec.Secrets {
		if secretstore.Validate("sandbox", slot, "read") != nil || value == "" || strings.ContainsRune(value, 0) {
			return nil, &Error{Op: "invalid stage secret"}
		}
		env = append(env, "EVALOOM_SECRET_"+strings.ToUpper(slot)+"="+value)
	}
	sort.Strings(env)
	return env, nil
}

// env -i would discard keys imported by Docker's name-only --env options.
// For keyed stages, remove every image variable and Docker's built-in variables
// instead, retaining only explicitly injected keys. Ordinary stage variables
// follow these options and overwrite any image defaults. No shell is involved.
func secretEntrypoint(imageEnv []string, spec SandboxSpec) ([]string, error) {
	remove := map[string]bool{"HOME": true, "HOSTNAME": true, "PATH": true, "TERM": true}
	for _, entry := range imageEnv {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !envName(key) {
			return nil, &Error{Op: "invalid image environment"}
		}
		remove[key] = true
	}
	for slot := range spec.Secrets {
		delete(remove, "EVALOOM_SECRET_"+strings.ToUpper(slot))
	}
	keys := make([]string, 0, len(remove))
	for key := range remove {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var args []string
	for _, key := range keys {
		args = append(args, "-u", key)
	}
	return args, nil
}

// Values live only in this create client's environment, never argv, files,
// shared Container state, sidecars, attach clients or later executions.
func (c *Container) createWithSecrets(ctx context.Context, env, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.ControlTimeout)
	defer cancel()
	cmd := c.command(ctx, args...)
	// Exclude similarly named RuntimeEnv entries so undeclared keys cannot be
	// imported accidentally. The explicit values win over trusted CLI wiring.
	filtered := cmd.Env[:0]
	for _, entry := range cmd.Env {
		if !strings.HasPrefix(entry, "EVALOOM_SECRET_") {
			filtered = append(filtered, entry)
		}
	}
	cmd.Env = append(filtered, env...)
	return cmd.Output()
}
