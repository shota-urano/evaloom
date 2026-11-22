package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandboxproxy"
)

// recordingChannel stands in for the Runner's handler: it answers
// cancel_check, allows authorize only for publish, and records each line.
type recordingChannel struct {
	kind  SandboxKind
	mu    sync.Mutex
	lines []string
}

func (r *recordingChannel) HandleChannelRequest(line []byte) []byte {
	r.mu.Lock()
	r.lines = append(r.lines, string(line))
	r.mu.Unlock()
	if strings.Contains(string(line), "authorize") {
		if r.kind != Publish {
			return nil
		}
		return []byte(`{"allowed":true}`)
	}
	return []byte(`{"cancelRequested":false}`)
}

func channelFixture(t *testing.T, mode string, kind SandboxKind) (*fixture, *recordingChannel) {
	t.Helper()
	f := stageFixture(t, mode, kind)
	f.config.ProxyImage = "proxy-image:explicit"
	f.config.ProxyBinary = filepath.Join(f.root, "runtime", "proxy")
	if err := os.WriteFile(f.config.ProxyBinary, []byte("trusted fixture binary"), 0700); err != nil {
		t.Fatal(err)
	}
	f.config.RuntimeEnv["RUNNER_TOKEN_CANARY"] = "unused"
	handler := &recordingChannel{kind: kind}
	f.spec.Channel = handler
	f.spec.Env["EVALOOM_CHANNEL"] = "/job-channel/channel.sock"
	return f, handler
}

func callIndex(calls []invocation, match func([]string) bool) int {
	return slices.IndexFunc(calls, func(c invocation) bool { return match(c.Args) })
}

func isCall(args []string, words ...string) bool {
	return len(args) >= len(words) && slices.Equal(args[:len(words)], words)
}

// spec 09 3.6 (Q232): the stage reaches the channel through a socket in its
// private volume; a network-none relay sidecar passes the requests over its
// stdio to the Runner's handler and returns the replies.
func TestStageChannelThroughVolumeAndRelaySidecar(t *testing.T) {
	for _, kind := range []SandboxKind{Prepare, Check, Publish} {
		t.Run(string(kind), func(t *testing.T) {
			f, handler := channelFixture(t, "channel-probe", kind)
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/usr/bin/env"})
			if err != nil {
				t.Fatal(err)
			}
			if err := waitProcess(t, p); err != nil {
				t.Fatal(err)
			}
			calls := f.calls(t)
			at := callIndex(calls, func(a []string) bool {
				return isCall(a, "container", "create") && !strings.HasSuffix(flag(a, "--name"), "-relay")
			})
			if at < 0 {
				t.Fatal("stage container was not created")
			}
			stage := calls[at].Args
			name := flag(stage, "--name")
			volume := name + "-channel"
			relay := name + "-relay"

			// No host socket bind mount; only the private channel volume.
			mounts := flagValues(stage, "--mount")
			want := "type=volume,source=" + volume + ",target=/job-channel,volume-nocopy,readonly"
			if !slices.Contains(mounts, want) || len(mounts) != len(f.spec.Mounts)+1 {
				t.Fatalf("missing private channel volume: %v", mounts)
			}
			for _, mount := range mounts {
				if strings.Contains(mount, "type=bind") && strings.Contains(mount, "channel") {
					t.Fatalf("host channel bind mount: %s", mount)
				}
			}
			if !slices.Contains(stage, "EVALOOM_CHANNEL=/job-channel/channel.sock") {
				t.Fatalf("EVALOOM_CHANNEL does not point into the volume: %v", stage)
			}

			// The sidecar: network none, read-only, no capabilities or
			// privileges, no environment and no Runner credentials.
			sidecar := callIndex(calls, func(a []string) bool { return isCall(a, "container", "create") && flag(a, "--name") == relay })
			if sidecar < 0 {
				t.Fatal("relay sidecar was not created")
			}
			args := calls[sidecar].Args
			for _, pair := range [][2]string{{"--network", "none"}, {"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges"}, {"--entrypoint", proxyExecutable}} {
				if flag(args, pair[0]) != pair[1] {
					t.Fatalf("sidecar %s=%q: %v", pair[0], flag(args, pair[0]), args)
				}
			}
			if !slices.Contains(args, "--read-only") || !slices.Contains(args, "--interactive") ||
				!slices.Contains(flagValues(args, "--mount"), "type=volume,source="+volume+",target="+sandboxproxy.ChannelDirectory+",volume-nocopy") ||
				slices.Contains(args, "--env") || slices.Contains(args, "-e") || slices.Contains(args, "--env-file") {
				t.Fatalf("unsafe relay sidecar: %v", args)
			}
			joined := strings.Join(args, "\n")
			for _, secret := range []string{"runner-secret", "RUNNER_TOKEN", "undeclared-secret", f.config.RunnerTokenFile, f.config.HostHome, f.config.DataDir} {
				if strings.Contains(joined, secret) {
					t.Fatalf("sidecar receives %q: %v", secret, args)
				}
			}
			for _, entry := range calls[sidecar].Env {
				if strings.Contains(entry, "runner-secret") {
					t.Fatalf("sidecar client environment carries a secret: %s", entry)
				}
			}

			// Ordering: volume and relay ready before the stage; both removed after it.
			order := []int{
				callIndex(calls, func(a []string) bool { return isCall(a, "volume", "create") && a[len(a)-1] == volume }),
				sidecar,
				callIndex(calls, func(a []string) bool { return isCall(a, "container", "exec", relay) }),
				callIndex(calls, func(a []string) bool { return isCall(a, "container", "create") && flag(a, "--name") == name }),
				callIndex(calls, func(a []string) bool { return isCall(a, "container", "rm") && a[len(a)-1] == name }),
				callIndex(calls, func(a []string) bool { return isCall(a, "container", "rm") && a[len(a)-1] == relay }),
				callIndex(calls, func(a []string) bool { return isCall(a, "volume", "rm") && a[len(a)-1] == volume }),
			}
			// The attach client logs asynchronously; it is ready before exec returns.
			started := callIndex(calls, func(a []string) bool {
				return isCall(a, "container", "start", "--attach", "--interactive") && a[len(a)-1] == relay
			})
			if slices.Contains(order, -1) || !slices.IsSorted(order) || started < 0 || started > order[3] {
				t.Fatalf("channel resources not created before / removed after the stage: %v", order)
			}

			// Replies come back through the socket; the sidecar drops
			// non-JSON and oversized lines without passing them on.
			authorize := "closed"
			if kind == Publish {
				authorize = `reply {"allowed":true}`
			}
			wantOut := `reply {"cancelRequested":false}` + "\n" + authorize + "\nclosed\nclosed\n"
			if f.stdout.String() != wantOut {
				t.Fatalf("stage saw %q; want %q", f.stdout.String(), wantOut)
			}
			if len(handler.lines) != 2 || handler.lines[0] != `{"op":"cancel_check"}` || !strings.Contains(handler.lines[1], `"authorize"`) {
				t.Fatalf("Runner received %q", handler.lines)
			}
		})
	}
}

// spec 09 3.6 item 6: no stage container without its channel, and Docker's
// reason is kept for the stage log.
func TestStageChannelRelayFailureCreatesNoStageContainer(t *testing.T) {
	for _, mode := range []string{"relay-volume-fail", "relay-create-fail", "relay-start-fail", "relay-check-fail"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := channelFixture(t, mode, Check)
			p, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
			if err == nil {
				_ = p.Wait()
				t.Fatal("stage started without its channel")
			}
			requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
			var runtime *RuntimeError
			if !strings.Contains(err.Error(), "stage channel relay unconfirmed") || !errors.As(err, &runtime) {
				t.Fatalf("error=%v lacks the runtime reason", err)
			}
			if mode != "relay-start-fail" && !strings.Contains(runtime.Stderr, "runtime diagnostic") {
				t.Fatalf("runtime reason lost: %+v", runtime)
			}
			calls := f.calls(t)
			if callIndex(calls, func(a []string) bool {
				return isCall(a, "container", "create") && !strings.HasSuffix(flag(a, "--name"), "-relay")
			}) >= 0 {
				t.Fatal("stage container created without a channel")
			}
			if mode != "relay-volume-fail" && callIndex(calls, func(a []string) bool { return isCall(a, "volume", "rm") }) < 0 {
				t.Fatal("partial channel volume was not removed")
			}
		})
	}
}

func TestAIStagesCannotReceiveStageChannel(t *testing.T) {
	for _, kind := range []SandboxKind{Work, Proposal} {
		t.Run(string(kind), func(t *testing.T) {
			f, _ := channelFixture(t, "normal", kind)
			_, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
			requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
			if len(f.calls(t)) != 0 {
				t.Fatal("AI stage channel reached runtime")
			}
		})
	}
}

func TestStageChannelRejectsUnsafeWiring(t *testing.T) {
	for _, change := range []string{"missing guest path", "overlapping guest path", "other socket name", "root directory",
		"request overlap", "no relay image", "relay binary in stage region"} {
		t.Run(change, func(t *testing.T) {
			f, _ := channelFixture(t, "normal", Publish)
			switch change {
			case "missing guest path":
				delete(f.spec.Env, "EVALOOM_CHANNEL")
			case "overlapping guest path":
				f.spec.Env["EVALOOM_CHANNEL"] = "/job/scratch/channel.sock"
			case "other socket name":
				f.spec.Env["EVALOOM_CHANNEL"] = "/job-channel/other.sock"
			case "root directory":
				f.spec.Env["EVALOOM_CHANNEL"] = "/channel.sock"
			case "request overlap":
				f.config.StageRequestPath = "/job-channel/request.json"
			case "no relay image":
				f.config.ProxyImage = ""
			case "relay binary in stage region":
				f.config.ProxyBinary = filepath.Join(f.config.StageMountRoots["/job/out"], "proxy")
				if err := os.WriteFile(f.config.ProxyBinary, nil, 0700); err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.sandbox(t).Run(context.Background(), f.spec, []string{"/bin/true"})
			requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
			if len(f.calls(t)) != 0 {
				t.Fatal("unsafe channel reached runtime")
			}
		})
	}
}
