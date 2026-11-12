package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/registry"
	"github.com/shota-urano/evaloom/backend/internal/sandboxproxy"
)

type modelDestinations []string

func (d modelDestinations) ModelAPIDestinations() []string { return d }

func networkFixture(t *testing.T, mode string, kind SandboxKind) *fixture {
	t.Helper()
	var f *fixture
	if kind == Harness {
		f = newFixture(t, mode)
	} else {
		f = stageFixture(t, mode, kind)
	}
	if kind == Harness || kind == Work || kind == Proposal {
		f.spec.Network = NetworkHarness
		f.spec.AllowedDestinations = []string{"model.example:443"}
		f.config.ModelAPI = modelDestinations{"model.example:443"}
	} else if kind == Prepare || kind == Publish {
		f.spec.Network = NetworkDeclared
		f.spec.AllowedDestinations = []string{"registry.example:443"}
		f.config.NetworkEnvironment = registry.Environment{ModelAPIs: map[string]bool{
			"model.example:443": true, "other-model.example:443": true,
		}}
	}
	f.config.ProxyImage = "proxy-image:explicit"
	f.config.ProxyBinary = filepath.Join(f.root, "runtime", "proxy")
	if err := os.WriteFile(f.config.ProxyBinary, []byte("trusted fixture binary"), 0700); err != nil {
		t.Fatal(err)
	}
	return f
}

func flag(args []string, key string) string {
	i := slices.Index(args, key)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func TestNetworkPoliciesWithFakeRuntime(t *testing.T) {
	for _, kind := range []SandboxKind{Harness, Prepare, Work, Check, Publish, Proposal} {
		t.Run(string(kind), func(t *testing.T) {
			f := networkFixture(t, "normal", kind)
			f.config.AllowEnv = append(f.config.AllowEnv, "HTTPS_PROXY", "NO_PROXY")
			f.spec.Env["HTTPS_PROXY"] = "http://host.docker.internal:9999"
			f.spec.Env["NO_PROXY"] = "*"
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/network-probe"})
			if err != nil {
				t.Fatal(err)
			}
			if err := waitProcess(t, p); err != nil {
				t.Fatal(err)
			}
			creates := map[string][]string{}
			var workload []string
			checked, removedHelpers, removedVolume := false, 0, false
			for _, call := range f.calls(t) {
				args := call.Args
				if args[0] == "container" && args[1] == "exec" {
					checked = true
				}
				if args[0] == "container" && args[1] == "create" {
					creates[flag(args, "--name")] = args
					if slices.Contains(args, "/network-probe") {
						workload = args
						if kind != Check && !checked {
							t.Fatal("workload created before proxy confirmation")
						}
					}
					if flag(args, "--cap-drop") != "ALL" || !slices.Contains(args, "no-new-privileges") || flag(args, "--pull") != "never" {
						t.Fatalf("weak container security: %v", args)
					}
				}
				if args[0] == "container" && args[1] == "rm" && (strings.HasSuffix(args[len(args)-1], "-ingress") || strings.HasSuffix(args[len(args)-1], "-egress")) {
					removedHelpers++
				}
				if args[0] == "volume" && args[1] == "rm" {
					removedVolume = true
				}
			}
			if workload == nil {
				t.Fatal("no workload")
			}
			network := flag(workload, "--network")
			if kind == Check {
				if network != "none" || len(creates) != 1 || checked {
					t.Fatal("Check gained a network, DNS or proxy")
				}
				for _, arg := range workload {
					if strings.Contains(arg, "PROXY=") || strings.Contains(arg, "proxy=") {
						t.Fatal("Check gained proxy environment")
					}
				}
			} else {
				ingress := creates[strings.TrimPrefix(network, "container:")]
				if !strings.HasPrefix(network, "container:") || flag(ingress, "--network") != "none" || len(creates) != 3 {
					t.Fatal("workload has external/host interface")
				}
				for _, args := range creates {
					if strings.HasSuffix(flag(args, "--name"), "-egress") {
						i := slices.Index(args, "egress")
						var destinations []string
						for _, arg := range args[i+2:] {
							if !strings.HasPrefix(arg, "--deny-ip=") {
								destinations = append(destinations, arg)
							}
						}
						if flag(args, "--network") != "bridge" || !slices.Equal(destinations, f.spec.AllowedDestinations) || !slices.Contains(args, "--deny-ip=127.0.0.1") {
							t.Fatal("wrong egress policy")
						}
					}
				}
				// The untrusted workload cannot reach the Unix socket or helper
				// executable, change namespaces, or replace the trusted relay.
				for _, arg := range workload {
					if strings.Contains(arg, "evaloom-ipc") || strings.Contains(arg, "evaloom-proxy") {
						t.Fatal("proxy control files exposed")
					}
				}
				index := slices.Index(workload, "-i")
				effective := map[string]string{}
				for _, arg := range workload[index+1 : len(workload)-1] {
					key, value, ok := strings.Cut(arg, "=")
					if ok {
						effective[key] = value
					}
				}
				if effective["HTTPS_PROXY"] != sandboxproxy.URL || effective["NO_PROXY"] != "" {
					t.Fatal("caller bypassed sandbox-owned proxy ENV")
				}
				if !checked || removedHelpers != 2 || !removedVolume {
					t.Fatal("proxy readiness or cleanup missing")
				}
			}
			// Model the effective routing table from the fake runtime's actual
			// creation graph. Both none and container:<ingress-none> have only
			// loopback; every direct host/GitHub/Internet probe is unreachable.
			for _, destination := range []string{"127.0.0.1:9999", "[::1]:9999", "host.docker.internal:9999", "172.17.0.1:9999", "github.com:443", "1.1.1.1:443"} {
				base := network
				if strings.HasPrefix(base, "container:") {
					base = flag(creates[strings.TrimPrefix(base, "container:")], "--network")
				}
				if base != "none" {
					t.Fatalf("%s has a direct route to %s", kind, destination)
				}
			}
		})
	}
}

func TestNetworkFailureNeverStartsWorkload(t *testing.T) {
	for _, kind := range []SandboxKind{Harness, Prepare, Work, Publish, Proposal} {
		for _, mode := range []string{"network-create-fail", "network-start-fail", "network-inspect-fail", "network-wrong-namespace", "network-not-running", "network-check-fail"} {
			t.Run(string(kind)+"/"+mode, func(t *testing.T) {
				f := networkFixture(t, mode, kind)
				p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/network-probe"})
				requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
				if p != nil {
					t.Fatal("process returned after failed network preparation")
				}
				for _, call := range f.calls(t) {
					if slices.Contains(call.Args, "/network-probe") {
						t.Fatal("workload reached runtime")
					}
				}
			})
		}
	}
}

func TestProgramStagesRejectAllRegisteredModelAPIs(t *testing.T) {
	for _, kind := range []SandboxKind{Prepare, Publish} {
		for _, destination := range []string{"model.example:443", "other-model.example:443"} {
			t.Run(string(kind)+"/"+destination, func(t *testing.T) {
				f := networkFixture(t, "normal", kind)
				f.spec.AllowedDestinations = append(f.spec.AllowedDestinations, destination)
				p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/network-probe"})
				var classified interface{ ErrorClass() model.ErrorClass }
				if !errors.As(err, &classified) || classified.ErrorClass() != model.ErrorConfig || p != nil || len(f.calls(t)) != 0 {
					t.Fatalf("model API reached runtime: process=%v error=%v", p, err)
				}
			})
		}
	}
}

func TestWorkAndProposalUseOnlySelectedAdapterDestinations(t *testing.T) {
	for _, kind := range []SandboxKind{Work, Proposal} {
		t.Run(string(kind), func(t *testing.T) {
			f := networkFixture(t, "normal", kind)
			f.spec.AllowedDestinations = []string{"profile-injected.example:443", "other-model.example:443"}
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/network-probe"})
			if err != nil {
				t.Fatal(err)
			}
			if err := waitProcess(t, p); err != nil {
				t.Fatal(err)
			}
			for _, call := range f.calls(t) {
				if slices.Contains(call.Args, "profile-injected.example:443") || slices.Contains(call.Args, "other-model.example:443") {
					t.Fatal("caller extended Adapter destinations")
				}
				if slices.Contains(call.Args, "egress") && !slices.Contains(call.Args, "model.example:443") {
					t.Fatal("selected Adapter destination missing")
				}
			}
		})
	}
}

func TestStageNetworkRejectsInvalidAuthorityBeforeRuntime(t *testing.T) {
	for _, kind := range stageKinds {
		for _, name := range []string{"wrong-policy", "local", "wildcard", "uppercase", "missing-source", "helper-in-stage"} {
			t.Run(string(kind)+"/"+name, func(t *testing.T) {
				f := networkFixture(t, "normal", kind)
				destinations := []string{"127.0.0.1:443"}
				switch name {
				case "wrong-policy":
					if kind == Prepare || kind == Publish || kind == Check {
						f.spec.Network = NetworkHarness
					} else {
						f.spec.Network = NetworkDeclared
					}
				case "wildcard":
					destinations = []string{"*.example.com:443"}
				case "uppercase":
					destinations = []string{"Model.example:443"}
				case "missing-source":
					f.config.ModelAPI, f.config.NetworkEnvironment = nil, nil
				case "helper-in-stage":
					f.config.ProxyBinary = filepath.Join(f.config.StageMountRoots[f.config.StagePaths.Out], "proxy")
					if err := os.WriteFile(f.config.ProxyBinary, nil, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if name == "local" || name == "wildcard" || name == "uppercase" {
					f.spec.AllowedDestinations = destinations
					f.config.ModelAPI = modelDestinations(destinations)
				}
				if kind == Check {
					// Even a valid declaration must not enable DNS or networking.
					f.spec.Network = NetworkDeclared
				}
				p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/network-probe"})
				if err == nil || p != nil || len(f.calls(t)) != 0 {
					t.Fatalf("invalid authority reached runtime: %v %v", p, err)
				}
			})
		}
	}
}

func TestProgramStageEmptyDeclarationNeedsNoProxy(t *testing.T) {
	for _, kind := range []SandboxKind{Prepare, Publish} {
		f := networkFixture(t, "normal", kind)
		f.spec.AllowedDestinations = nil
		f.config.ProxyBinary, f.config.ProxyImage = "", ""
		f.config.NetworkEnvironment = nil
		p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/network-probe"})
		if err != nil {
			t.Fatal(err)
		}
		if err := waitProcess(t, p); err != nil {
			t.Fatal(err)
		}
		if flag(createCall(t, f), "--network") != "none" {
			t.Fatal("empty declaration gained network")
		}
	}
}

func TestNetworkRejectsInvalidPolicyBeforeRuntime(t *testing.T) {
	for _, edit := range []func(*fixture){
		func(f *fixture) { f.spec.Kind = "test_run"; f.spec.ConfigDir = "" },
		func(f *fixture) { f.spec.Network = "setup" },
		func(f *fixture) { f.spec.AllowedDestinations = nil },
		func(f *fixture) { f.spec.AllowedDestinations = []string{"github.com:443"} },
		func(f *fixture) { f.spec.AllowedDestinations = []string{"api.github.com:443"} },
		func(f *fixture) { f.spec.AllowedDestinations = []string{"host.docker.internal:443"} },
		func(f *fixture) { f.spec.AllowedDestinations = []string{"127.0.0.1:443"} },
		func(f *fixture) { f.spec.AllowedDestinations = []string{"*.example.com:443"} },
		func(f *fixture) { f.config.ProxyImage = "" },
		func(f *fixture) { f.config.ProxyBinary = "" },
		func(f *fixture) { f.config.ProxyBinary = f.spec.Worktree },
		func(f *fixture) {
			f.config.ProxyBinary = filepath.Join(f.spec.Worktree, "proxy")
			_ = os.WriteFile(f.config.ProxyBinary, nil, 0700)
		},
		func(f *fixture) { f.spec.Network = NetworkNone },
	} {
		f := networkFixture(t, "normal", Harness)
		edit(f)
		p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/network-probe"})
		requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
		if p != nil || len(f.calls(t)) != 0 {
			t.Fatal("invalid policy invoked runtime")
		}
	}
}
