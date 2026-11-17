package stage

import (
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

func TestPrepareOmitted(t *testing.T) {
	var profile contract.ProfileDefinition
	if First(profile) != model.StageWork {
		t.Fatal("undeclared prepare selected")
	}
	if _, err := Assemble(profile, model.StagePrepare, contract.RequestPaths{}, nil, sandbox.ResourceLimits{}); err == nil {
		t.Fatal("undeclared prepare assembled")
	}
	profile.Stages.Prepare = &contract.ProgramStage{}
	if First(profile) != model.StagePrepare {
		t.Fatal("declared prepare omitted")
	}
}

func TestStageMountsAndNetwork(t *testing.T) {
	paths := contract.RequestPaths{Profile: "/profile", Workspace: "/workspace", Out: "/out", Inputs: "/inputs", Records: "/records", Scratch: "/scratch"}
	roots := map[string]string{}
	for _, guest := range []string{paths.Profile, paths.Workspace, paths.Out, paths.Inputs, paths.Records, paths.Scratch} {
		roots[guest] = "/host" + guest
	}
	var profile contract.ProfileDefinition
	profile.Stages.Prepare = &contract.ProgramStage{Network: &contract.Network{Allow: []string{"source.example.com:443"}}}
	profile.Stages.Publish.Network = &contract.Network{Allow: []string{"output.example.com:443"}}
	// Even a bypassed registration must not grant networking to check.
	profile.Stages.Check.Network = profile.Stages.Publish.Network
	limits := sandbox.ResourceLimits{CPU: 2, MemoryBytes: 1024, Time: time.Minute}
	for _, tt := range []struct {
		kind         model.Stage
		mounts       map[string]bool
		network      sandbox.NetworkPolicy
		destinations []string
	}{
		{model.StagePrepare, map[string]bool{"/profile": false, "/workspace": true, "/out": true, "/scratch": true}, sandbox.NetworkDeclared, []string{"source.example.com:443"}},
		{model.StageWork, map[string]bool{"/workspace": true, "/out": true, "/scratch": true, "/inputs": false, "/records": false}, sandbox.NetworkNone, nil},
		{model.StageCheck, map[string]bool{"/profile": false, "/workspace": false, "/out": true, "/scratch": true, "/inputs": false}, sandbox.NetworkNone, nil},
		{model.StagePublish, map[string]bool{"/profile": false, "/workspace": false, "/out": true, "/scratch": true, "/inputs": false}, sandbox.NetworkDeclared, []string{"output.example.com:443"}},
	} {
		t.Run(string(tt.kind), func(t *testing.T) {
			spec, err := Assemble(profile, tt.kind, paths, roots, limits)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, mount := range spec.Mounts {
				got[mount.GuestPath] = mount.Writable
				if mount.HostPath != roots[mount.GuestPath] {
					t.Fatal("mount uses another stage's source")
				}
			}
			if len(spec.Mounts) != len(tt.mounts) || !maps.Equal(got, tt.mounts) {
				t.Fatalf("mount visibility/write access = %v; want %v", got, tt.mounts)
			}
			if spec.Network != tt.network || !slices.Equal(spec.AllowedDestinations, tt.destinations) || spec.Limits != limits || spec.Kind != sandbox.SandboxKind(tt.kind) {
				t.Fatal("wrong stage network, kind or resource limits")
			}
			if len(spec.Secrets) != 0 || spec.ConfigDir != "" || spec.Worktree != "" {
				t.Fatal("assembly granted unrelated capabilities")
			}
			if len(spec.AllowedDestinations) != 0 {
				spec.AllowedDestinations[0] = "changed.example.com:443"
				program, _ := Program(profile, tt.kind)
				if program.Network.Allow[0] != tt.destinations[0] {
					t.Fatal("assembly aliases Profile destinations")
				}
			}
		})
	}
	delete(roots, paths.Workspace)
	if _, err := Assemble(profile, model.StageCheck, paths, roots, limits); err == nil {
		t.Fatal("missing workspace accepted")
	}
	if _, err := Assemble(profile, model.Stage("unknown"), paths, roots, limits); err == nil {
		t.Fatal("unknown stage accepted")
	}
}

func TestStageSecrets(t *testing.T) {
	store := secretstore.New(t.TempDir())
	for _, key := range []struct{ slot, access string }{{"source", "read"}, {"delivery", "write"}, {"undeclared", "read"}} {
		if err := store.Set("fixture", key.slot, key.access, []byte(key.slot+"-value")); err != nil {
			t.Fatal(err)
		}
	}
	var profile contract.ProfileDefinition
	profile.Stages.Prepare = &contract.ProgramStage{Secrets: []contract.Secret{{Slot: "source", Access: "read"}}}
	profile.Stages.Publish.Secrets = []contract.Secret{{Slot: "delivery", Access: "write"}, {Slot: "source", Access: "read"}}
	for _, tt := range []struct {
		kind model.Stage
		want map[string]string
	}{
		{model.StagePrepare, map[string]string{"EVALOOM_SECRET_SOURCE": "source-value"}},
		{model.StageWork, nil},
		{model.StageCheck, nil},
		{model.StagePublish, map[string]string{"EVALOOM_SECRET_DELIVERY": "delivery-value", "EVALOOM_SECRET_SOURCE": "source-value"}},
	} {
		t.Run(string(tt.kind), func(t *testing.T) {
			keys, err := ResolveSecrets(store, "fixture", profile, tt.kind)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(keys.Environment(), tt.want) {
				t.Fatal("stage received wrong keys")
			}
		})
	}
	for _, key := range []contract.Secret{{Slot: "delivery", Access: "read"}, {Slot: "delivery", Access: "write"}, {Slot: "missing", Access: "read"}} {
		profile.Stages.Prepare.Secrets = []contract.Secret{{Slot: "source", Access: "read"}, key}
		keys, err := ResolveSecrets(store, "fixture", profile, model.StagePrepare)
		var config *secretstore.ConfigError
		if !errors.As(err, &config) || config.ErrorClass() != model.ErrorConfig || len(keys.Environment()) != 0 {
			t.Fatal("prepare received partial, missing or write keys")
		}
	}
	profile.Stages.Check.Secrets = profile.Stages.Publish.Secrets
	if keys, err := ResolveSecrets(store, "fixture", profile, model.StageCheck); err == nil || len(keys.Environment()) != 0 {
		t.Fatal("check accepted key declarations")
	}
}
