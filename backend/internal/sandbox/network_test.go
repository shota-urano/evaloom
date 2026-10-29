package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandboxproxy"
)

func networkFixture(t *testing.T, mode string, kind SandboxKind) *fixture {
	t.Helper()
	f := newFixture(t, mode)
	f.spec.Kind = kind
	if kind != Harness {
		f.spec.ConfigDir = ""
	}
	if kind == Harness {
		f.spec.Network = NetworkHarness
		f.spec.AllowedDestinations = []string{"model.example:443"}
	}
	if kind == TestSetup {
		f.spec.Network = NetworkSetup
		f.spec.AllowedDestinations = []string{"registry.example:443"}
	}
	if kind == TestRun {
		f.spec.Network = NetworkNone
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
	for _, kind := range []SandboxKind{Harness, TestSetup, TestRun} {
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
						if kind != TestRun && !checked {
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
			if kind == TestRun {
				if network != "none" || len(creates) != 1 || checked {
					t.Fatal("test_run gained a network or proxy")
				}
			} else {
				ingress := creates[strings.TrimPrefix(network, "container:")]
				if !strings.HasPrefix(network, "container:") || flag(ingress, "--network") != "none" || len(creates) != 3 {
					t.Fatal("workload has external/host interface")
				}
				for _, args := range creates {
					if strings.HasSuffix(flag(args, "--name"), "-egress") {
						if flag(args, "--network") != "bridge" || !slices.Contains(args, f.spec.AllowedDestinations[0]) {
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
	for _, mode := range []string{"network-create-fail", "network-start-fail", "network-inspect-fail", "network-wrong-namespace", "network-not-running", "network-check-fail"} {
		t.Run(mode, func(t *testing.T) {
			f := networkFixture(t, mode, Harness)
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

func TestNetworkRejectsInvalidPolicyBeforeRuntime(t *testing.T) {
	for _, edit := range []func(*fixture){
		func(f *fixture) { f.spec.Kind = TestRun; f.spec.ConfigDir = "" },
		func(f *fixture) { f.spec.Network = NetworkSetup },
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
