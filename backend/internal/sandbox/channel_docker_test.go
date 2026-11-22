//go:build security_docker

package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fixedChannel struct{ lines chan string }

func (c fixedChannel) HandleChannelRequest(line []byte) []byte {
	c.lines <- string(line)
	return []byte(`{"cancelRequested":true}`)
}

// spec 09 3.6 (Q232) on a real Docker daemon (Docker Desktop on macOS
// included): a check stage connects to EVALOOM_CHANNEL, a socket in its
// private volume served by the network-none relay sidecar, and receives the
// Runner's cancel_check reply. Opt-in (security_docker), outside make verify:
//
//	EVALOOM_SECURITY_TEST_IMAGE=evaloom-harness:local EVALOOM_SECURITY_PROXY_IMAGE=evaloom-proxy:local \
//	  go test -tags security_docker -run TestDockerStageChannel ./internal/sandbox
func TestDockerStageChannel(t *testing.T) {
	image, proxyImage := os.Getenv("EVALOOM_SECURITY_TEST_IMAGE"), os.Getenv("EVALOOM_SECURITY_PROXY_IMAGE")
	if image == "" || proxyImage == "" {
		t.Fatal("EVALOOM_SECURITY_TEST_IMAGE (with node) and EVALOOM_SECURITY_PROXY_IMAGE are required")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	f := stageFixture(t, "normal", Check)
	f.config.RuntimePath, f.config.Image, f.config.ProxyImage = docker, image, proxyImage
	f.config.ControlTimeout = 10 * time.Second
	f.config.RuntimeEnv = map[string]string{}
	for _, key := range []string{"HOME", "PATH", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		if value, ok := os.LookupEnv(key); ok {
			f.config.RuntimeEnv[key] = value
		}
	}
	arch, err := exec.Command(docker, "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "evaloom-channel-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f.config.ProxyBinary = filepath.Join(dir, "proxy")
	build := exec.Command("go", "build", "-o", f.config.ProxyBinary, "../../cmd/evaloom-sandbox-proxy")
	build.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "GOOS=") || strings.HasPrefix(kv, "GOARCH=") || strings.HasPrefix(kv, "CGO_ENABLED=")
	}), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build proxy: %v %s", err, out)
	}
	resources := func() []string {
		var names []string
		for _, args := range [][]string{{"container", "ls", "--all", "--format", "{{.Names}}"}, {"volume", "ls", "--format", "{{.Name}}"}} {
			out, err := exec.Command(docker, args...).Output()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range strings.Fields(string(out)) {
				if strings.HasPrefix(name, "evaloom-") && (strings.HasSuffix(name, "-relay") || strings.HasSuffix(name, "-channel")) {
					names = append(names, name)
				}
			}
		}
		return names
	}
	before := resources()

	handler := fixedChannel{lines: make(chan string, 4)}
	f.spec.Channel = handler
	f.spec.Env["EVALOOM_CHANNEL"] = "/job-channel/channel.sock"
	f.spec.Env["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	f.spec.Limits = ResourceLimits{CPU: 1, MemoryBytes: 512 << 20, Time: 90 * time.Second}
	script := `const s = require("net").connect(process.env.EVALOOM_CHANNEL, () => s.write('{"op":"cancel_check"}\n'));
let reply = ""; s.on("data", d => { reply += d; if (reply.includes("\n")) { process.stdout.write("channel reply " + reply); s.end(); } });
s.on("error", e => { console.error("channel error " + e.code); process.exit(3); });`
	p, err := f.sandbox(t).RunControlled(context.Background(), f.spec, []string{"node", "-e", script})
	if err != nil {
		t.Fatalf("stage launch: %v (%+v %s)", err, errorsAs(err), func() string {
			if r := errorsAs(err); r != nil {
				return r.Stderr + r.Detail
			}
			return ""
		}())
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("stage: %v stdout=%q stderr=%q", err, f.stdout.String(), f.stderr.String())
	}
	if code, ok := p.ExitCode(); !ok || code != 0 {
		t.Fatalf("exit=%d/%t stdout=%q stderr=%q", code, ok, f.stdout.String(), f.stderr.String())
	}
	if got := f.stdout.String(); got != "channel reply {\"cancelRequested\":true}\n" {
		t.Fatalf("stage stdout=%q stderr=%q", got, f.stderr.String())
	}
	select {
	case line := <-handler.lines:
		if line != `{"op":"cancel_check"}` {
			t.Fatalf("Runner received %q", line)
		}
	default:
		t.Fatal("request did not reach the Runner")
	}
	if after := resources(); !slices.Equal(after, before) {
		t.Fatalf("relay sidecar or channel volume left behind: before=%v after=%v", before, after)
	}
	t.Logf("stage output: %s", f.stdout.String())
}

func errorsAs(err error) *RuntimeError {
	var runtime *RuntimeError
	errors.As(err, &runtime)
	return runtime
}
