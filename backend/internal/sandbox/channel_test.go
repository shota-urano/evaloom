package sandbox

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func testChannelListener(t *testing.T) *net.UnixListener {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ec-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener
}

func TestStageChannelRuntimeMountAndEnvironment(t *testing.T) {
	for _, kind := range []SandboxKind{Prepare, Check, Publish} {
		t.Run(string(kind), func(t *testing.T) {
			f := stageFixture(t, "env-probe", kind)
			listener := testChannelListener(t)
			f.spec.ChannelPath = listener.Addr().String()
			ctx := WithStageChannel(context.Background(), kind, listener)
			p, err := f.sandbox(t).Run(ctx, f.spec, []string{"/usr/bin/env"})
			if err != nil {
				t.Fatal(err)
			}
			if err := waitProcess(t, p); err != nil {
				t.Fatal(err)
			}
			source, err := filepath.EvalSymlinks(f.spec.ChannelPath)
			if err != nil {
				t.Fatal(err)
			}
			mounts := flagValues(createCall(t, f), "--mount")
			want := "type=bind,source=" + source + ",target=/job/channel.sock,readonly"
			if !slices.Contains(mounts, want) || len(mounts) != len(f.spec.Mounts)+1 {
				t.Fatalf("missing exclusive channel mount: %v", mounts)
			}
			if !strings.Contains(f.stdout.String(), "EVALOOM_CHANNEL=/job/channel.sock\n") ||
				strings.Contains(f.stdout.String(), f.spec.ChannelPath) || strings.Contains(f.stdout.String(), "RUNNER_TOKEN") {
				t.Fatalf("wrong channel environment: %s", f.stdout.String())
			}
			// Process completion does not close Runner's listener prematurely;
			// the enclosing stage owns it through collection and reporting.
			if conn, err := net.Dial("unix", f.spec.ChannelPath); err != nil {
				t.Fatal(err)
			} else {
				conn.Close()
			}
		})
	}
}

func TestAIStagesCannotReceiveStageChannel(t *testing.T) {
	for _, kind := range []SandboxKind{Work, Proposal} {
		t.Run(string(kind), func(t *testing.T) {
			f := stageFixture(t, "env-probe", kind)
			listener := testChannelListener(t)
			f.spec.ChannelPath = listener.Addr().String()
			ctx := WithStageChannel(context.Background(), kind, listener)
			_, err := f.sandbox(t).Run(ctx, f.spec, []string{"/bin/true"})
			requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
			if len(f.calls(t)) != 0 {
				t.Fatal("AI stage channel reached runtime")
			}
		})
	}
}

func TestStageChannelRejectsUnboundClosedReplacedAndUnsafeSockets(t *testing.T) {
	for _, change := range []string{"no binding", "wrong stage", "other listener", "closed", "replaced",
		"missing guest path", "overlapping guest path", "symlink", "protected source"} {
		t.Run(change, func(t *testing.T) {
			f := stageFixture(t, "normal", Publish)
			listener := testChannelListener(t)
			f.spec.ChannelPath = listener.Addr().String()
			ctx := WithStageChannel(context.Background(), Publish, listener)
			switch change {
			case "no binding":
				ctx = context.Background()
			case "wrong stage":
				ctx = WithStageChannel(context.Background(), Check, listener)
			case "other listener":
				ctx = WithStageChannel(context.Background(), Publish, testChannelListener(t))
			case "closed":
				listener.SetUnlinkOnClose(false)
				listener.Close()
			case "replaced":
				listener.Close()
				if err := os.WriteFile(f.spec.ChannelPath, []byte("not a socket"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing guest path":
				delete(f.spec.Env, "EVALOOM_CHANNEL")
			case "overlapping guest path":
				f.spec.Env["EVALOOM_CHANNEL"] = "/job/scratch/channel.sock"
			case "symlink":
				alias := filepath.Join(filepath.Dir(f.spec.ChannelPath), "alias")
				if err := os.Symlink(f.spec.ChannelPath, alias); err != nil {
					t.Fatal(err)
				}
				f.spec.ChannelPath = alias
			case "protected source":
				f.config.RunnerTokenFile = f.spec.ChannelPath
			}
			_, err := f.sandbox(t).Run(ctx, f.spec, []string{"/bin/true"})
			requireClass(t, err, model.ExecutionFailed, model.ErrorSandbox)
			if len(f.calls(t)) != 0 {
				t.Fatal("unsafe channel reached runtime")
			}
		})
	}
}
