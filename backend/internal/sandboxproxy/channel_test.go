package sandboxproxy

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type relayFixture struct {
	socket   string
	requests *bufio.Reader // what the sidecar passes to the Runner (its stdout)
	replies  *io.PipeWriter
	served   chan error
}

func newRelayFixture(t *testing.T) *relayFixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "cr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, ChannelName)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	f := &relayFixture{socket: socket, requests: bufio.NewReader(outReader), replies: inWriter, served: make(chan error, 1)}
	go func() { f.served <- ServeChannel(listener, inReader, outWriter) }()
	t.Cleanup(func() {
		inWriter.Close()
		outReader.Close()
		listener.Close()
	})
	return f
}

func (f *relayFixture) dial(t *testing.T) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() { conn.Close() })
	return conn
}

func (f *relayFixture) passed(t *testing.T) string {
	t.Helper()
	line, err := f.requests.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(line, "\n")
}

func readReply(t *testing.T, conn net.Conn) (string, bool) {
	t.Helper()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", false
	}
	return strings.TrimSuffix(line, "\n"), true
}

// spec 09 3.6 item 4: the sidecar checks only the form (one line of JSON
// within the limit) and passes nothing else to the Runner.
func TestServeChannelPassesOnlyBoundedJSONLines(t *testing.T) {
	f := newRelayFixture(t)
	for _, request := range []string{"not json", `{"op":"cancel_check"} {}`, `{"op":`, strings.Repeat(" ", ChannelLineLimit+1)} {
		conn := f.dial(t)
		io.WriteString(conn, request+"\n")
		if reply, ok := readReply(t, conn); ok {
			t.Fatalf("%q answered %q", request, reply)
		}
	}
	// A request exactly at the limit, and an unterminated one, after them.
	conn := f.dial(t)
	limit := `{"op":"cancel_check"}` + strings.Repeat(" ", ChannelLineLimit-len(`{"op":"cancel_check"}`))
	io.WriteString(conn, limit+"\n")
	if got := f.passed(t); got != limit {
		t.Fatalf("Runner received %d bytes", len(got))
	}
	io.WriteString(f.replies, `{"cancelRequested":true}`+"\n")
	if reply, ok := readReply(t, conn); !ok || reply != `{"cancelRequested":true}` {
		t.Fatalf("reply=%q ok=%v", reply, ok)
	}
	partial := f.dial(t).(*net.UnixConn)
	io.WriteString(partial, `{"op":"cancel_check"}`)
	partial.CloseWrite()
	if reply, ok := readReply(t, partial); ok {
		t.Fatalf("unterminated request answered %q", reply)
	}
}

// Replies match requests in order; the Runner's reject closes only that
// connection; concurrent requests are both passed before either is answered.
func TestServeChannelOrderedRepliesAndReject(t *testing.T) {
	f := newRelayFixture(t)
	first, second := f.dial(t), f.dial(t)
	io.WriteString(first, `{"n":1}`+"\n")
	if got := f.passed(t); got != `{"n":1}` {
		t.Fatalf("passed %q", got)
	}
	io.WriteString(second, `{"n":2}`+"\n")
	if got := f.passed(t); got != `{"n":2}` {
		t.Fatalf("passed %q", got)
	}
	io.WriteString(f.replies, "null\n{\"r\":2}\n")
	if reply, ok := readReply(t, first); ok {
		t.Fatalf("rejected request answered %q", reply)
	}
	if reply, ok := readReply(t, second); !ok || reply != `{"r":2}` {
		t.Fatalf("second reply=%q ok=%v", reply, ok)
	}
}

// The Runner closing its side (the sidecar's stdin) stops the relay.
func TestServeChannelStopsWhenRunnerCloses(t *testing.T) {
	f := newRelayFixture(t)
	conn := f.dial(t)
	f.replies.Close()
	select {
	case <-f.served:
	case <-time.After(3 * time.Second):
		t.Fatal("relay kept running without the Runner")
	}
	io.WriteString(conn, `{"op":"cancel_check"}`+"\n")
	if reply, ok := readReply(t, conn); ok {
		t.Fatalf("answered %q without the Runner", reply)
	}
}
