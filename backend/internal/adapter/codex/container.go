package codex

import (
	"context"
	"io"
	"maps"
	"slices"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// ContainerLaunch binds the existing container implementation to an execution's
// trusted GitDir, network policy and resource limits. No defaults are invented.
// One launcher/adapter may serve multiple executions via resolve.
func ContainerLaunch(config sandbox.ContainerConfig, resolve func(adapter.SandboxEnv) (sandbox.SandboxSpec, error)) Launch {
	config.RuntimeEnv = maps.Clone(config.RuntimeEnv)
	config.AllowEnv = slices.Clone(config.AllowEnv)
	return func(ctx context.Context, env adapter.SandboxEnv, args []string, stdout, stderr io.Writer) (sandbox.Process, error) {
		if resolve == nil {
			return nil, &sandbox.Error{Op: "Codex sandbox scope required"}
		}
		spec, err := resolve(env)
		if err != nil {
			return nil, err
		}
		if spec.Kind != sandbox.Harness || spec.Worktree != env.Worktree || spec.ConfigDir != env.ConfigDir ||
			!maps.Equal(spec.Env, env.Env) || !slices.Contains(config.AllowEnv, "CODEX_HOME") {
			return nil, &sandbox.Error{Op: "Codex sandbox scope mismatch"}
		}
		local := config
		local.Stdout, local.Stderr = stdout, stderr
		container, err := sandbox.NewContainer(local)
		if err != nil {
			return nil, err
		}
		return container.Run(ctx, spec, args)
	}
}
