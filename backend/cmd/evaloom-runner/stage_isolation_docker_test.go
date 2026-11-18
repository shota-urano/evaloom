//go:build security_docker

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/runner"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	st "github.com/shota-urano/evaloom/backend/internal/securitytest"
)

// Spec 14 3.2 on the real Sandbox (Mac mini). Not part of make verify:
//
//	cd backend
//	EVALOOM_SECURITY_TEST_IMAGE=evaloom-security-boundary:local \
//	EVALOOM_SECURITY_PROXY_IMAGE=evaloom-security-boundary:local \
//	EVALOOM_SECURITY_DECLARED=probe-destination.example:8080@<public-ip>:8080 \
//	EVALOOM_SECURITY_UNDECLARED=probe-undeclared.example:8081@<public-ip>:8081 \
//	EVALOOM_SECURITY_MODEL=probe-model.example:8082@<public-ip>:8082 \
//	go test -tags=security_docker ./cmd/evaloom-runner -run '^TestStageIsolationDocker$' -count=1 -v -timeout 30m
//
// Each destination is NAME:PORT@LISTEN: the test serves the destination on
// LISTEN (a non-loopback address this host owns) and the caller makes NAME
// resolve, for the Sandbox proxy, to a public address that reaches it
// (Q150; the proxy refuses private and special-use addresses). Missing
// prerequisites fail the test as unavailable; nothing is skipped.

// TestIsolationDockerRuntimeHelper is the runtime CLI wrapper of the real
// run: it records every create command (for CreateArgsVerdict), applies the
// negative control's Loosening, and then executes the real Docker CLI with
// the same arguments, environment and standard streams.
func TestIsolationDockerRuntimeHelper(t *testing.T) {
	docker := os.Getenv("EVALOOM_ISOLATION_DOCKER")
	if docker == "" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	if len(args) > 1 && args[0] == "container" && args[1] == "create" {
		if raw := os.Getenv("EVALOOM_FAKE_LOOSEN"); raw != "" {
			var l st.Loosening
			if err := json.Unmarshal([]byte(raw), &l); err != nil {
				os.Exit(93)
			}
			args = l.Loosen(args, runner.DefaultProposalPaths)
		}
		if parsed, err := st.ParseCreateArgs(args); err == nil && parsed.Name != "" {
			data, _ := json.Marshal(args)
			if err := os.WriteFile(filepath.Join(os.Getenv("EVALOOM_ISOLATION_RECORD"), parsed.Name+".json"), data, 0600); err != nil {
				os.Exit(94)
			}
		}
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "EVALOOM_ISOLATION_") || strings.HasPrefix(kv, "EVALOOM_FAKE_LOOSEN=")
	})
	err := syscall.Exec(docker, append([]string{docker}, args...), env)
	fmt.Fprintln(os.Stderr, err)
	os.Exit(95)
}

type isolationDestination struct {
	endpoint st.Endpoint
	server   *st.Destination
}

func requireDestination(t *testing.T, variable string, sandboxCheck interface{ Check(context.Context) error }) isolationDestination {
	t.Helper()
	value := os.Getenv(variable)
	name, listen, ok := strings.Cut(value, "@")
	host, port, err := net.SplitHostPort(name)
	n, portErr := strconv.Atoi(port)
	if !ok || err != nil || portErr != nil {
		t.Fatalf("実行不能（スキップではない）: %s must be NAME:PORT@LISTEN, got %q", variable, value)
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatalf("実行不能（スキップではない）: %s: %v", variable, err)
	}
	d := st.NewDestination(listener)
	t.Cleanup(func() { d.Close() })
	d.SetFile(st.IsolationProbePath, []byte("isolation probe destination\n"))
	st.Prerequisites{Sandbox: sandboxCheck, RequireDestination: true, Destination: d}.Require(context.Background(), t)
	return isolationDestination{endpoint: st.Endpoint{Host: host, Port: n}, server: d}
}

func (d isolationDestination) records() []string {
	var uris []string
	for _, r := range d.server.Requests() {
		uris = append(uris, r.Path+"?"+r.Query)
	}
	return uris
}

type dockerCheck struct {
	docker, image string
	env           []string
}

func (c dockerCheck) Check(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.docker, "run", "--rm", "--pull", "never", "--network", "none", "--entrypoint", "/usr/bin/env", c.image,
		"-i", "PATH=/usr/local/bin:/usr/bin:/bin", "python3", "-c", "import socket, urllib.request")
	cmd.Env = c.env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("実行不能: Docker or image unavailable: %v %s", err, out)
	}
	return nil
}

func hostAddress(t *testing.T) string {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addresses {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLoopback() && !ip.IP.IsLinkLocalUnicast() {
			return ip.IP.String()
		}
	}
	t.Fatal("実行不能（スキップではない）: no non-loopback host address for the Control API")
	return ""
}

type dockerIsolation struct {
	docker, image, proxyImage, proxyBinary string
	runtimeEnv                             map[string]string
	declared, undeclared, model            isolationDestination
	secrets                                st.ProbeSecrets
	other                                  string
}

func newDockerIsolation(t *testing.T) *dockerIsolation {
	t.Helper()
	e := &dockerIsolation{image: os.Getenv("EVALOOM_SECURITY_TEST_IMAGE"), proxyImage: os.Getenv("EVALOOM_SECURITY_PROXY_IMAGE"),
		runtimeEnv: map[string]string{}, secrets: st.NewProbeSecrets(), other: st.NewCanary()}
	if e.image == "" || e.proxyImage == "" {
		t.Fatal("実行不能（スキップではない）: EVALOOM_SECURITY_TEST_IMAGE and EVALOOM_SECURITY_PROXY_IMAGE are required")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("実行不能（スキップではない）: %v", err)
	}
	if e.docker, err = filepath.Abs(docker); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, key := range []string{"HOME", "PATH", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		if value, ok := os.LookupEnv(key); ok {
			e.runtimeEnv[key] = value
			env = append(env, key+"="+value)
		}
	}
	check := dockerCheck{docker: e.docker, image: e.image, env: env}
	st.Prerequisites{Sandbox: check}.Require(context.Background(), t)
	arch, err := exec.Command(e.docker, "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatalf("実行不能（スキップではない）: %v", err)
	}
	dir, err := os.MkdirTemp("/private/tmp", "e7d-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	e.proxyBinary = filepath.Join(dir, "proxy")
	build := exec.Command("go", "build", "-o", e.proxyBinary, "../evaloom-sandbox-proxy")
	build.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "GOOS=") || strings.HasPrefix(kv, "GOARCH=") || strings.HasPrefix(kv, "CGO_ENABLED=")
	}), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("実行不能（スキップではない）: build proxy: %v %s", err, out)
	}
	e.declared = requireDestination(t, "EVALOOM_SECURITY_DECLARED", check)
	e.undeclared = requireDestination(t, "EVALOOM_SECURITY_UNDECLARED", check)
	e.model = requireDestination(t, "EVALOOM_SECURITY_MODEL", check)
	return e
}

func (e *dockerIsolation) hidden(f *executionFixture, otherTask string) map[string][]string {
	home, _ := os.UserHomeDir()
	root := filepath.Dir(f.config.DataDir)
	owner := []string{home, filepath.Join(home, ".ssh"), filepath.Join(home, "Library", "Keychains"), "/var/run/docker.sock", "/run/docker.sock",
		filepath.Join(home, ".docker", "run", "docker.sock")}
	if agent := os.Getenv("SSH_AUTH_SOCK"); agent != "" {
		owner = append(owner, agent)
	}
	return map[string][]string{
		st.RowManagementData: {filepath.Join(root, "control.db"), f.config.SecretsDir, f.config.TokenFile, f.config.MicroHarnessAuthFiles["codex"],
			filepath.Join(root, "control-data"), filepath.Join(f.config.DataDir, "executions")},
		st.RowOwnerHome:       owner,
		st.RowOtherWorkspaces: {filepath.Join(otherTask, "marker")},
	}
}

// controlAPI serves the fixture's Control Plane (and its access log) on a
// host address too, and returns every base URL the probe must fail to reach.
func controlAPI(t *testing.T, f *executionFixture) []string {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(hostAddress(t), "0"))
	if err != nil {
		t.Fatalf("実行不能（スキップではない）: %v", err)
	}
	server := &http.Server{Handler: f.server.Config.Handler, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	_, loopPort, _ := net.SplitHostPort(f.server.Listener.Addr().String())
	_, hostPort, _ := net.SplitHostPort(listener.Addr().String())
	// Positive control: both listeners answer and are logged.
	for _, base := range []string{f.server.URL, "http://" + listener.Addr().String()} {
		response, err := http.Get(base + "/api/tasks?positive-control")
		if err != nil {
			t.Fatalf("実行不能（スキップではない）: Control API positive control: %v", err)
		}
		response.Body.Close()
	}
	return []string{"http://127.0.0.1:" + loopPort, "http://[::1]:" + loopPort, "http://" + listener.Addr().String(),
		"http://host.docker.internal:" + loopPort, "http://host.docker.internal:" + hostPort, "http://gateway.docker.internal:" + loopPort}
}

func (e *dockerIsolation) targets(f *executionFixture, otherTask string) st.IsolationTargets {
	return st.IsolationTargets{ControlAPI: controlAPI(f.t, f), Hidden: e.hidden(f, otherTask),
		Declared: e.declared.endpoint, Undeclared: e.undeclared.endpoint, Model: e.model.endpoint,
		Secrets: e.secrets, UndeclaredSecret: e.other, SecretFiles: []string{"/evaloom/request.json", "/proc/self/environ", "/proc/1/environ"},
		ProfileFile: runner.DefaultProposalPaths.Profile + "/evaloom/profile.yaml", Workspace: runner.DefaultProposalPaths.Workspace}
}

func (e *dockerIsolation) records(f *executionFixture) st.TrustedRecords {
	f.mu.Lock()
	accesses := slices.Clone(f.accesses)
	f.mu.Unlock()
	return st.TrustedRecords{ControlAPI: accesses, Destinations: map[string][]string{
		st.RowDeclaredDest: e.declared.records(), st.RowUndeclaredDest: e.undeclared.records(), st.RowModelAPI: e.model.records()}}
}

// runtimeWiring makes a fixture run on the real Docker runtime through
// TestIsolationDockerRuntimeHelper and returns where create commands are
// recorded.
func (e *dockerIsolation) runtimeWiring(t *testing.T) func(*executionFixture) string {
	return func(fixture *executionFixture) string {
		record := filepath.Join(filepath.Dir(fixture.config.DataDir), "created")
		if err := os.Mkdir(record, 0700); err != nil {
			t.Fatal(err)
		}
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		wrapper := filepath.Join(filepath.Dir(fixture.config.DataDir), "runtime-docker")
		if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+shellQuote(binary)+" -test.run=^TestIsolationDockerRuntimeHelper$ -- \"$@\"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		c := &fixture.config.Container
		c.RuntimePath, c.Image, c.ProxyImage, c.ProxyBinary = wrapper, e.image, e.proxyImage, e.proxyBinary
		c.RuntimeEnv = map[string]string{"EVALOOM_ISOLATION_DOCKER": e.docker, "EVALOOM_ISOLATION_RECORD": record}
		for key, value := range e.runtimeEnv {
			c.RuntimeEnv[key] = value
		}
		fixture.wait = 10 * time.Minute
		return record
	}
}

// run executes one Execution on the real Sandbox and returns the violations.
func (e *dockerIsolation) run(t *testing.T, loosen *st.Loosening) []st.Violation {
	t.Helper()
	var f *executionFixture
	r := newIsolationRun(t, isolationOptions{loosen: loosen, secrets: e.secrets, other: e.other,
		network: []string{e.declared.endpoint.Authority()}, modelAPI: []string{e.model.endpoint.Authority()},
		targets: func(fixture *executionFixture, other string) st.IsolationTargets {
			f = fixture
			return e.targets(fixture, other)
		},
		runtime: e.runtimeWiring(t)})
	violations, stages := r.verdict(t, func() st.TrustedRecords { return e.records(f) })
	if !slices.Equal(stages, []string{"check", "prepare", "publish", "work"}) {
		t.Fatalf("judged stages %v", stages)
	}
	return violations
}

// proposal runs the probe in the Proposal Sandbox built by the production
// spec and Container, on the real runtime, with the model destination.
func (e *dockerIsolation) proposal(t *testing.T, loosen *st.Loosening) []st.Violation {
	t.Helper()
	// A fixture supplies the Control Plane, data directory and other task
	// the probe must not reach; no Execution is started.
	f := newHarnessFixture(t, "probe", inlineHarness("success"))
	otherTask := filepath.Join(f.config.WorkspaceRoot, "other-task")
	if err := os.MkdirAll(otherTask, 0700); err != nil {
		t.Fatal(err)
	}
	d := st.IsolationDeclarations{ModelAPI: true}
	targets := e.targets(f, otherTask)
	plan, rules := st.IsolationPlan(st.IsolationProposal, d, targets)
	source, err := st.ProbeAccessSource()
	if err != nil {
		t.Fatal(err)
	}
	attempt := filepath.Join(f.config.DataDir, "proposals", "j1", "proposal-1")
	dirs := map[string]string{"out": filepath.Join(attempt, "out"), "inputs": filepath.Join(attempt, "inputs"), "scratch": filepath.Join(attempt, "scratch"), "record": filepath.Join(attempt, "record")}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(attempt, "runtime-docker")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+shellQuote(binary)+" -test.run=^TestIsolationDockerRuntimeHelper$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runtimeEnv := map[string]string{"EVALOOM_ISOLATION_DOCKER": e.docker, "EVALOOM_ISOLATION_RECORD": dirs["record"]}
	for key, value := range e.runtimeEnv {
		runtimeEnv[key] = value
	}
	if loosen != nil {
		raw, _ := json.Marshal(loosen)
		runtimeEnv["EVALOOM_FAKE_LOOSEN"] = string(raw)
	}
	home, _ := os.UserHomeDir()
	paths := runner.DefaultProposalPaths
	var stdout, stderr bytes.Buffer
	box, err := sandbox.NewContainer(sandbox.ContainerConfig{RuntimePath: wrapper, Image: e.image, ProxyImage: e.proxyImage, ProxyBinary: e.proxyBinary,
		RuntimeEnv: runtimeEnv, StagePaths: paths,
		StageMountRoots: map[string]string{paths.Out: dirs["out"], paths.Inputs: dirs["inputs"], paths.Scratch: dirs["scratch"]},
		HostHome:        home, DataDir: f.config.DataDir, RunnerTokenFile: f.config.TokenFile, ConfigDirRoot: filepath.Join(attempt, "config"),
		ControlTimeout: 30 * time.Second, ModelAPI: modelDestinations{e.model.endpoint.Authority()}, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := runner.BuildProposalSandboxSpec(runner.ProposalHarnessInput{Paths: paths, InputsHost: dirs["inputs"], OutHost: dirs["out"], ScratchHost: dirs["scratch"],
		Env: map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8"}, Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 512 << 20, Time: 5 * time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	process, err := box.RunControlled(context.Background(), spec, []string{"python3", "-c", string(source), "--inline-plan", string(plan), paths.Out + "/proposal-probe.json"})
	if err != nil {
		t.Fatalf("実行不能（スキップではない）: Proposal Sandbox: %v", err)
	}
	if err := process.Wait(); err != nil {
		t.Fatalf("Proposal probe: %v %s %s", err, stdout.String(), stderr.String())
	}
	report, err := os.ReadFile(filepath.Join(dirs["out"], "proposal-probe.json"))
	if err != nil {
		t.Fatal(err)
	}
	violations := st.ObservationVerdict(st.IsolationProposal, report, rules, d, e.records(f))
	created, _ := filepath.Glob(filepath.Join(dirs["record"], "*.json"))
	w := st.ArgsWiring{Paths: paths, Protected: map[string][]string{st.RowManagementData: {f.config.SecretsDir, f.config.TokenFile}},
		Ancestors: map[string][]string{st.RowManagementData: {f.config.DataDir}, st.RowOwnerHome: {home}}}
	judged := false
	for _, record := range created {
		raw, _ := os.ReadFile(record)
		var args []string
		if json.Unmarshal(raw, &args) != nil {
			t.Fatalf("record %s", record)
		}
		parsed, err := st.ParseCreateArgs(args)
		if err != nil || st.StageOf(parsed, paths) != st.IsolationProposal {
			continue // proxy sidecars
		}
		judged = true
		violations = append(violations, st.CreateArgsVerdict(st.IsolationProposal, args, d, w)...)
	}
	if !judged {
		t.Fatal("Proposal create command not recorded")
	}
	return violations
}

type modelDestinations []string

func (m modelDestinations) ModelAPIDestinations() []string { return m }

// TestStageIsolationDocker: the whole 3.2 table on the real Sandbox, then
// the negative controls of the acceptance criteria, each of which must make
// its row fail.
func TestStageIsolationDocker(t *testing.T) {
	e := newDockerIsolation(t)
	t.Run("isolated", func(t *testing.T) {
		if v := e.run(t, nil); len(v) != 0 {
			t.Fatalf("3.2 violated: %v", v)
		}
	})
	t.Run("isolated_proposal", func(t *testing.T) {
		if v := e.proposal(t, nil); len(v) != 0 {
			t.Fatalf("3.2 (Proposal) violated: %v", v)
		}
	})
	for _, tc := range []struct {
		name   string
		loosen st.Loosening
		row    string
	}{
		{"loopback_allowed", st.Loosening{Stage: st.IsolationCheck, Network: "host"}, st.RowControlAPI},
		{"key_to_work", st.Loosening{Stage: st.IsolationWork, Env: map[string]string{st.SecretEnvName(st.ProbeReadSlot): e.secrets.Read}}, st.RowProbeRead},
		{"write_key_to_prepare", st.Loosening{Stage: st.IsolationPrepare, Env: map[string]string{st.SecretEnvName(st.ProbeWriteSlot): e.secrets.Write}}, st.RowProbeWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loosen := tc.loosen
			if v := e.run(t, &loosen); !slices.Contains(st.Rows(v), tc.row) {
				t.Fatalf("loosened %s not detected as %s: %v", tc.loosen.Stage, tc.row, v)
			}
		})
	}
	t.Run("proposal_loopback_allowed", func(t *testing.T) {
		if v := e.proposal(t, &st.Loosening{Stage: st.IsolationProposal, Network: "host"}); !slices.Contains(st.Rows(v), st.RowControlAPI) {
			t.Fatalf("loosened proposal not detected: %v", v)
		}
	})
}
