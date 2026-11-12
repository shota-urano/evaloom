package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

func storedStageKeys(t *testing.T, f *fixture) *secretstore.Store {
	t.Helper()
	s := secretstore.New(f.config.DataDir)
	for _, key := range []struct{ slot, access, value string }{
		{"source", "read", "read-key\nsecond line\r\n"},
		{"destination", "write", "write-key-with-$(literal)="},
		{"undeclared", "write", "undeclared-stored-key"},
	} {
		if err := s.Set("test", key.slot, key.access, []byte(key.value)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// Model the existing Runner preflight followed by the stage layer's transport
// boundary. Environment goes through SandboxSpec.Secrets, never Env or files.
func runStoredStage(ctx context.Context, c *Container, s *secretstore.Store, spec SandboxSpec, declarations []contract.Secret) (Process, error) {
	resolved, err := s.ForStage("test", model.Stage(spec.Kind), declarations)
	if err != nil {
		return nil, err
	}
	spec.Secrets = make(map[string]string)
	for name, value := range resolved.Environment() {
		slot := strings.ToLower(strings.TrimPrefix(name, "EVALOOM_SECRET_"))
		spec.Secrets[slot] = value
	}
	return c.Run(ctx, spec, []string{"/usr/bin/env"})
}

func TestStoredKeysReachOnlyDeclaredStagesWithoutArgumentsOrFiles(t *testing.T) {
	for _, kind := range stageKinds {
		t.Run(string(kind), func(t *testing.T) {
			f := stageFixture(t, "env-probe", kind)
			s := storedStageKeys(t, f)
			t.Setenv("EVALOOM_SECRET_HOST", "host-only-key")
			declarations := []contract.Secret(nil)
			want := map[string]string{}
			if kind == Prepare || kind == Publish {
				declarations = append(declarations, contract.Secret{Slot: "source", Access: "read"})
				want["EVALOOM_SECRET_SOURCE"] = "read-key\nsecond line\r\n"
			}
			if kind == Publish {
				declarations = append(declarations, contract.Secret{Slot: "destination", Access: "write"})
				want["EVALOOM_SECRET_DESTINATION"] = "write-key-with-$(literal)="
			}
			// Even trusted runtime wiring must not override a selected value or
			// add an undeclared key to the create client.
			f.config.RuntimeEnv["EVALOOM_SECRET_SOURCE"] = "runtime-source"
			f.config.RuntimeEnv["EVALOOM_SECRET_EXTRA"] = "runtime-extra"
			p, err := runStoredStage(context.Background(), f.sandbox(t), s, f.spec, declarations)
			if err != nil {
				t.Fatal(err)
			}
			if err := waitProcess(t, p); err != nil {
				t.Fatal(err)
			}
			output := f.stdout.String()
			for name, value := range want {
				if !strings.Contains(output, name+"="+value+"\n") {
					t.Fatalf("declared key %s did not reach workload intact", name)
				}
			}
			if strings.Count(output, "EVALOOM_SECRET_") != len(want) {
				t.Fatal("undeclared keys reached workload")
			}
			for _, forbidden := range []string{"host-only-key", "runtime-source", "runtime-extra", "image-key", "image-source", "IMAGE_SECRET=", "HOSTNAME=", "TERM=", "undeclared-stored-key", "undeclared-secret"} {
				if strings.Contains(output, forbidden) {
					t.Fatalf("forbidden environment reached workload: %s", forbidden)
				}
			}
			if !strings.Contains(output, "PATH=/usr/bin:/bin\n") || !strings.Contains(output, "TZ=UTC\n") {
				t.Fatal("stage environment lost its explicit values")
			}
			for _, call := range f.calls(t) {
				create := len(call.Args) > 1 && call.Args[0] == "container" && call.Args[1] == "create"
				for name, value := range want {
					if strings.Contains(strings.Join(call.Args, "\x00"), value) {
						t.Fatal("key value exposed in runtime arguments")
					}
					if slices.Contains(call.Env, name+"="+value) != create {
						t.Fatal("key must be supplied only to the create client")
					}
				}
			}
			if got := flagValues(createCall(t, f), "--env"); len(got) != len(want) {
				t.Fatal("runtime did not receive exactly the declared variable names")
			}
			// Inspect all host files that the fake runtime would bind into the
			// guest. The key store and fake runtime records must remain outside.
			roots := []string{f.spec.ConfigDir}
			for _, mount := range f.spec.Mounts {
				roots = append(roots, mount.HostPath)
			}
			for _, root := range roots {
				if root == "" {
					continue
				}
				err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
					if err != nil || entry.IsDir() {
						return err
					}
					data, err := os.ReadFile(path)
					for _, value := range []string{"read-key", "write-key-with-$(literal)=", "undeclared-stored-key"} {
						if bytes.Contains(data, []byte(value)) {
							t.Fatal("key value present in a guest-visible file")
						}
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestStoredKeyPreflightPreventsSandboxCreation(t *testing.T) {
	for _, test := range []struct {
		name string
		kind SandboxKind
		keys []contract.Secret
	}{
		{"prepare stored write", Prepare, []contract.Secret{{Slot: "destination", Access: "read"}}},
		{"prepare declared write", Prepare, []contract.Secret{{Slot: "source", Access: "write"}}},
		{"publish missing after valid", Publish, []contract.Secret{{Slot: "source", Access: "read"}, {Slot: "missing", Access: "write"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := stageFixture(t, "normal", test.kind)
			s := storedStageKeys(t, f)
			p, err := runStoredStage(context.Background(), f.sandbox(t), s, f.spec, test.keys)
			var config *secretstore.ConfigError
			if p != nil || !errors.As(err, &config) || config.ErrorClass() != model.ErrorConfig || len(f.calls(t)) != 0 {
				t.Fatalf("invalid stage must fail before runtime with config_error: %v", err)
			}
		})
	}
}

func TestSandboxRejectsKeysBeforeAnyRuntimeCall(t *testing.T) {
	for _, kind := range []SandboxKind{Work, Check, Proposal, Harness} {
		for _, controlled := range []bool{false, true} {
			f := stageFixture(t, "normal", kind)
			f.spec.Secrets = map[string]string{"source": "private-value"}
			c := f.sandbox(t)
			run := c.Run
			if controlled {
				run = c.RunControlled
			}
			_, err := run(context.Background(), f.spec, []string{"/bin/true"})
			requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
			if len(f.calls(t)) != 0 || strings.Contains(err.Error(), "private-value") {
				t.Fatal("forbidden stage keys reached runtime or error")
			}
		}
	}
	for _, keys := range []map[string]string{{"BAD": "private-value"}, {"../key": "private-value"}, {"source": ""}, {"source": "private\x00value"}} {
		f := stageFixture(t, "normal", Publish)
		f.spec.Secrets = keys
		_, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
		requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
		if len(f.calls(t)) != 0 {
			t.Fatal("invalid key reached runtime")
		}
	}
}

func TestStageMountCannotExposeKeyStore(t *testing.T) {
	for _, kind := range stageKinds {
		f := stageFixture(t, "normal", kind)
		storedStageKeys(t, f)
		keys := filepath.Join(f.config.DataDir, "secrets", "test")
		f.config.StageMountRoots["/job/out"] = keys
		_, err := NewContainer(f.config)
		requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
		if len(f.calls(t)) != 0 {
			t.Fatal("key store mount reached runtime")
		}
	}
}

func TestStageKeysDoNotRemainInReusableContainer(t *testing.T) {
	f := stageFixture(t, "env-probe", Publish)
	s := storedStageKeys(t, f)
	c := f.sandbox(t)
	p, err := runStoredStage(context.Background(), c, s, f.spec, []contract.Secret{{Slot: "source", Access: "read"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitProcess(t, p); err != nil {
		t.Fatal(err)
	}
	f.stdout.Reset()
	// Check and Publish have the same mount permissions, so reuse the trusted
	// container wiring while starting a fresh, keyless Check sandbox.
	f.spec.Kind = Check
	p, err = runStoredStage(context.Background(), c, s, f.spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitProcess(t, p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.stdout.String(), "EVALOOM_SECRET_") || strings.Contains(f.stdout.String(), "read-key") {
		t.Fatal("prior publish key reached the following check stage")
	}
	calls := f.calls(t)
	creates := 0
	for _, call := range calls {
		if len(call.Args) > 1 && call.Args[0] == "container" && call.Args[1] == "create" {
			creates++
		}
		if creates == 2 && slices.Contains(call.Env, "EVALOOM_SECRET_SOURCE=read-key\nsecond line\r\n") {
			t.Fatal("prior key remained in the reusable runtime environment")
		}
	}
}
