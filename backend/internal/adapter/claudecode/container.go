package claudecode

import (
	"context"
	"io"
	"maps"
	"slices"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// Model API and subscription-auth destinations adopted in spec 00, Q71.
// Keep them in this adapter package; GitHub access belongs to Git Publisher.
const (
	anthropicAPIDestination     = "api.anthropic.com:443"
	anthropicConsoleDestination = "console.anthropic.com:443"
	claudeAIDestination         = "claude.ai:443"
)

func (*harness) ModelAPIDestinations() []string {
	return modelAPIDestinations()
}

func modelAPIDestinations() []string {
	return []string{anthropicAPIDestination, anthropicConsoleDestination, claudeAIDestination}
}

// ContainerLaunch binds the existing container implementation to an execution's
// trusted GitDir, network policy and resource limits. The adapter owns
// the exact model/auth destinations; caller-supplied destinations are replaced.
// One launcher/adapter may serve multiple executions via resolve.
func ContainerLaunch(config sandbox.ContainerConfig, resolve func(adapter.SandboxEnv) (sandbox.SandboxSpec, error)) Launch {
	return containerLaunch(config, resolve, func(ctx context.Context, config sandbox.ContainerConfig, spec sandbox.SandboxSpec, args []string) (sandbox.Process, error) {
		container, err := sandbox.NewContainer(config)
		if err != nil {
			return nil, err
		}
		return container.RunControlled(ctx, spec, args)
	})
}

func containerLaunch(config sandbox.ContainerConfig, resolve func(adapter.SandboxEnv) (sandbox.SandboxSpec, error), run func(context.Context, sandbox.ContainerConfig, sandbox.SandboxSpec, []string) (sandbox.Process, error)) Launch {
	config.RuntimeEnv = maps.Clone(config.RuntimeEnv)
	config.AllowEnv = slices.Clone(config.AllowEnv)
	return func(ctx context.Context, env adapter.SandboxEnv, args []string, stdout, stderr io.Writer) (sandbox.Process, error) {
		if resolve == nil {
			return nil, &sandbox.Error{Op: "Claude Code sandbox scope required"}
		}
		spec, err := resolve(env)
		if err != nil {
			return nil, err
		}
		if spec.Kind != sandbox.Harness || spec.Worktree != env.Worktree || spec.ConfigDir != env.ConfigDir ||
			!maps.Equal(spec.Env, env.Env) || !slices.Contains(config.AllowEnv, "CLAUDE_CONFIG_DIR") {
			return nil, &sandbox.Error{Op: "Claude Code sandbox scope mismatch"}
		}
		// NetworkNone remains useful for offline executions. Never inherit setup
		// hosts or arbitrary destinations from Runner's reusable SandboxSpec.
		if spec.Network != "" && spec.Network != sandbox.NetworkNone && spec.Network != sandbox.NetworkHarness {
			return nil, &sandbox.Error{Op: "invalid harness network policy"}
		}
		spec.AllowedDestinations = nil
		if spec.Network == sandbox.NetworkHarness {
			spec.AllowedDestinations = modelAPIDestinations()
		}
		// Use the verified private configuration volume from ctx.
		spec.ConfigWritable = true
		local := config
		local.Stdout, local.Stderr = stdout, stderr
		return run(ctx, local, spec, args)
	}
}
