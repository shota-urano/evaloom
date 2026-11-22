package sandboxproxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
)

// Stage channel relay (spec 09 3.6, Q232). The stage connects to a Unix socket
// inside a private volume; the network-none sidecar passes each request line,
// one at a time, over its own stdio to the Runner and returns the reply. The
// sidecar never interprets a request: it checks only that it is one line of
// JSON within the size limit (spec 05 3.7).
const (
	ChannelDirectory = "/evaloom-channel"
	ChannelName      = "channel.sock"
	ChannelSocket    = ChannelDirectory + "/" + ChannelName
	// ChannelLineLimit excludes the terminating LF (spec 05 3.7: 64 KiB).
	ChannelLineLimit = 64 << 10
)

// ChannelReject is the Runner's stdio reply meaning "close this connection
// without replying" (an op the stage may not use, or a malformed request).
// It is valid JSON on one line and never a contract 7.4 reply.
var ChannelReject = []byte("null")

var errChannelLine = errors.New("invalid channel line")

// ReadChannelLine returns one LF-terminated line without its LF. It never
// buffers beyond the limit, and an unterminated final line is an error.
func ReadChannelLine(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return nil, io.EOF
		}
		return nil, errChannelLine
	}
	line = line[:len(line)-1]
	if len(line) > ChannelLineLimit {
		return nil, errChannelLine
	}
	return line, nil
}

// NewChannelReader bounds reads to one line plus its LF.
func NewChannelReader(r io.Reader) *bufio.Reader { return bufio.NewReaderSize(r, ChannelLineLimit+1) }

// ValidChannelLine is the only check the sidecar makes on a request.
func ValidChannelLine(line []byte) bool {
	return len(line) <= ChannelLineLimit && json.Valid(line)
}

// ServeChannel accepts stage connections on listener and relays requests to
// out (the sidecar's stdout), reading replies from in (its stdin). Each
// request is written as one line; the Runner answers every line with one
// line, in the order the requests were written, so a reply belongs to the
// oldest unanswered request. Requests are not held back while an earlier one
// is answered (two publish authorizations may reach the Control Plane
// together). It returns when the Runner side closes or misbehaves, or the
// listener fails; every stage connection is closed then.
func ServeChannel(listener net.Listener, in io.Reader, out io.Writer) error {
	r := &channelRelay{out: out, done: make(chan struct{}), listener: listener, connections: map[net.Conn]struct{}{}}
	go r.readReplies(in)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer r.stop()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		r.mu.Lock()
		if r.stopped {
			r.mu.Unlock()
			conn.Close()
			return nil
		}
		r.connections[conn] = struct{}{}
		r.mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			r.serve(conn)
			conn.Close()
			r.mu.Lock()
			delete(r.connections, conn)
			r.mu.Unlock()
		}()
	}
}

type channelRelay struct {
	out         io.Writer
	listener    net.Listener
	done        chan struct{}
	mu          sync.Mutex // guards out, pending, connections and stopped
	pending     []chan []byte
	connections map[net.Conn]struct{}
	stopped     bool
}

func (r *channelRelay) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	r.stopped = true
	close(r.done)
	r.listener.Close()
	for conn := range r.connections {
		conn.Close()
	}
}

func (r *channelRelay) readReplies(in io.Reader) {
	reader := NewChannelReader(in)
	for {
		reply, err := ReadChannelLine(reader)
		r.mu.Lock()
		if err != nil || !json.Valid(reply) || len(r.pending) == 0 {
			r.mu.Unlock()
			r.stop() // The Runner closed, or answered what was never asked.
			return
		}
		waiter := r.pending[0]
		r.pending = r.pending[1:]
		r.mu.Unlock()
		waiter <- append([]byte(nil), reply...)
	}
}

func (r *channelRelay) serve(conn net.Conn) {
	reader := NewChannelReader(conn)
	for {
		line, err := ReadChannelLine(reader)
		if err != nil || !ValidChannelLine(line) {
			return // Not forwarded: the stage gets no reply and the connection closes.
		}
		waiter := make(chan []byte, 1)
		r.mu.Lock()
		if r.stopped {
			r.mu.Unlock()
			return
		}
		r.pending = append(r.pending, waiter)
		_, err = r.out.Write(append(append([]byte(nil), line...), '\n'))
		r.mu.Unlock()
		if err != nil {
			r.stop()
			return
		}
		var reply []byte
		select {
		case reply = <-waiter:
		case <-r.done:
			return
		}
		if string(reply) == string(ChannelReject) {
			return
		}
		if _, err := conn.Write(append(reply, '\n')); err != nil {
			return
		}
	}
}
