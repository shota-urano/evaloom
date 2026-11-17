// Package stage assembles the per-stage capabilities of a pinned Profile.
// Runner owns file preparation and credential lifetime; sandbox enforces these
// capabilities against trusted roots and the selected Adapter's destinations.
package stage

import (
	"fmt"
	"slices"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

func First(profile contract.ProfileDefinition) model.Stage {
	if profile.Stages.Prepare != nil {
		return model.StagePrepare
	}
	return model.StageWork
}

// Program selects only a declared program stage. Work has no program command.
func Program(profile contract.ProfileDefinition, kind model.Stage) (*contract.ProgramStage, error) {
	switch kind {
	case model.StagePrepare:
		if profile.Stages.Prepare != nil {
			return profile.Stages.Prepare, nil
		}
	case model.StageWork:
		return nil, nil
	case model.StageCheck:
		return &profile.Stages.Check, nil
	case model.StagePublish:
		return &profile.Stages.Publish, nil
	}
	return nil, fmt.Errorf("stage: undeclared or unsupported stage")
}

// ResolveSecrets delegates stored-access checks to the existing secret store.
// In particular, a read declaration cannot downgrade a stored write key.
func ResolveSecrets(store *secretstore.Store, harness string, profile contract.ProfileDefinition, kind model.Stage) (secretstore.Resolved, error) {
	program, err := Program(profile, kind)
	if err != nil {
		return secretstore.Resolved{}, err
	}
	var declarations []contract.Secret
	if program != nil {
		declarations = program.Secrets
	}
	return store.ForStage(harness, kind, declarations)
}

// WorkFiles is the complete selection passed to harnesscontext.Read. That
// reader and harnessenv project only these pinned files into configuration.
func WorkFiles(profile contract.ProfileDefinition) []string {
	w := profile.Stages.Work
	files := slices.Clone(w.Instructions)
	files = append(files, w.Skills...)
	return append(files, w.Settings...)
}

// Assemble builds contract 6.2 mounts and network policy. Paths, roots and
// resource limits come from trusted wiring, without choosing deployment values.
// Env, the request file, channel and configuration are attached by Runner.
// Work starts offline; Runner enables NetworkHarness only with the selected
// Adapter's declaration. Profile destinations can never extend that policy.
func Assemble(profile contract.ProfileDefinition, kind model.Stage, paths contract.RequestPaths, roots map[string]string, limits sandbox.ResourceLimits) (sandbox.SandboxSpec, error) {
	program, err := Program(profile, kind)
	if err != nil {
		return sandbox.SandboxSpec{}, err
	}
	spec := sandbox.SandboxSpec{Kind: sandbox.SandboxKind(kind), Limits: limits, Network: sandbox.NetworkNone}
	add := func(guest string, writable bool) error {
		if guest == "" || roots[guest] == "" {
			return fmt.Errorf("stage: missing mount path")
		}
		spec.Mounts = append(spec.Mounts, sandbox.Mount{HostPath: roots[guest], GuestPath: guest, Writable: writable})
		return nil
	}
	type permission struct {
		path     string
		writable bool
	}
	permissions := []permission{{paths.Out, true}, {paths.Scratch, true}, {paths.Workspace, kind == model.StagePrepare || kind == model.StageWork}}
	if kind != model.StageWork {
		permissions = append(permissions, permission{paths.Profile, false})
	}
	if kind != model.StagePrepare {
		permissions = append(permissions, permission{paths.Inputs, false})
	}
	if kind == model.StageWork {
		permissions = append(permissions, permission{paths.Records, false})
	}
	for _, mount := range permissions {
		if err := add(mount.path, mount.writable); err != nil {
			return sandbox.SandboxSpec{}, err
		}
	}
	if (kind == model.StagePrepare || kind == model.StagePublish) && program.Network != nil {
		spec.Network = sandbox.NetworkDeclared
		spec.AllowedDestinations = slices.Clone(program.Network.Allow)
	}
	return spec, nil
}
