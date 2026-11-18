package securitytest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// DestinationOperationPath receives the publish stage's one operation.
const DestinationOperationPath = "/operations"

// DestinationRequest is recorded before any routing, including requests the
// destination answers with 404. It is independent of files a stage writes.
type DestinationRequest struct {
	Method, Path, RemoteAddr string
	Body                     []byte
}

// Destination is the test external destination of spec 14 3.0 (Q150): a
// server reachable from the Sandbox network at a non-loopback address. The
// caller supplies the listener (for example on the address of a test
// container or of a Softnet-permitted IP) and the name resolution; this
// type never contacts real external services.
type Destination struct {
	listener net.Listener
	server   *http.Server
	mu       sync.Mutex
	requests []DestinationRequest
	files    map[string][]byte
}

// NewDestination serves on listener until Close.
func NewDestination(listener net.Listener) *Destination {
	d := &Destination{listener: listener, files: map[string][]byte{}}
	d.server = &http.Server{Handler: http.HandlerFunc(d.serveHTTP), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = d.server.Serve(listener) }()
	return d
}

func (d *Destination) Close() error { return d.server.Close() }

// Authority is host:port, the form of a Profile's network.allow entry when
// the address itself is declared.
func (d *Destination) Authority() string { return d.listener.Addr().String() }

func (d *Destination) URL(path string) string { return "http://" + d.Authority() + path }

// SetFile makes GET path return data (for example a script the prepare stage
// fetches in spec 14 3.3).
func (d *Destination) SetFile(path string, data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files[path] = append([]byte(nil), data...)
}

func (d *Destination) Requests() []DestinationRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	requests := append([]DestinationRequest(nil), d.requests...)
	for i := range requests {
		requests[i].Body = append([]byte(nil), requests[i].Body...)
	}
	return requests
}

// Operations counts the publish operations that reached the destination.
func (d *Destination) Operations() int {
	count := 0
	for _, r := range d.Requests() {
		if r.Method == http.MethodPost && r.Path == DestinationOperationPath {
			count++
		}
	}
	return count
}

// Check fails as unavailable when the destination is a loopback or an
// unspecified address: the Sandbox blocks loopback, so such a destination
// could make a denied connection look like an isolation success.
func (d *Destination) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d == nil {
		return errors.New("実行不能: テスト用の外部の宛先が必要")
	}
	addr, ok := d.listener.Addr().(*net.TCPAddr)
	if !ok || addr.IP == nil || addr.IP.IsLoopback() || addr.IP.IsUnspecified() {
		return fmt.Errorf("実行不能: テスト用の外部の宛先はループバック以外のアドレスが必要 (%s)", d.listener.Addr())
	}
	return nil
}

func (d *Destination) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	d.mu.Lock()
	d.requests = append(d.requests, DestinationRequest{Method: r.Method, Path: r.URL.Path, RemoteAddr: r.RemoteAddr, Body: body})
	file, found := d.files[r.URL.Path]
	d.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == DestinationOperationPath:
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && found:
		_, _ = w.Write(file)
	default:
		http.NotFound(w, r)
	}
}
