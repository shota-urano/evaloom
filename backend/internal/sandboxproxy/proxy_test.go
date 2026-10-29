package sandboxproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDestinationAndPublicAddresses(t *testing.T) {
	for _, value := range []string{"*.example.com:443", "example.com", "example.com:0443", "Example.com:443", "https://example.com:443", "127.0.0.1:443", "[::1]:443", "host.docker.internal:443", "gateway.docker.internal:443", "localhost.local:443", "a.home.arpa:443", "a..com:443", "a.com:65536", "a.com:0", "a.com.:443"} {
		if _, err := Destination(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	for _, value := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "0.0.0.0", "10.0.0.1", "172.17.0.1", "192.168.65.254", "169.254.169.254", "100.64.0.1", "198.18.0.1", "224.0.0.1", "fc00::1", "fe80::1", "64:ff9b::7f00:1", "2002:7f00:1::", "2001:db8::1"} {
		if PublicIP(netip.MustParseAddr(value)) {
			t.Errorf("public %q", value)
		}
	}
	for _, value := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !PublicIP(netip.MustParseAddr(value)) {
			t.Errorf("blocked public %q", value)
		}
	}
}

// Exercise the actual HTTP parser, Unix socket relay and policy handler.
// Only the last dial is replaced: it records the validated, pinned public IP
// and connects to a local fixture. No request can reach an external network.
func proxyFixture(t *testing.T, p *Proxy) *httptest.Server {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "evaloom-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "proxy.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	egress := &http.Server{Handler: p, ReadHeaderTimeout: time.Second}
	go func() { _ = egress.Serve(listener) }()
	t.Cleanup(func() { _ = egress.Close() })
	server := httptest.NewServer(ingress(time.Second, socket))
	t.Cleanup(server.Close)
	return server
}

func TestProxyAllowedHTTPAndConnectThroughPrivateSocket(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Remove") != "" {
			t.Error("proxy headers forwarded")
		}
		if r.Host != "model.example:80" {
			t.Errorf("Host=%s", r.Host)
		}
		_, _ = io.WriteString(w, "allowed fixture")
	}))
	defer upstream.Close()
	p, err := New([]string{"model.example:80", "model.example:443"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	var dials atomic.Int32
	p.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "1.1.1.1:80" && address != "1.1.1.1:443" {
			t.Errorf("unvalidated dial %s", address)
		}
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
	}
	proxy := proxyFixture(t, p)
	proxyURL, _ := url.Parse(proxy.URL)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	req, _ := http.NewRequest("GET", "http://model.example/", nil)
	req.Host = "model.example:80"
	// Keep URL and Host identical for the strict forward-proxy check.
	req.URL.Host = req.Host
	req.Header.Set("Proxy-Authorization", "secret")
	req.Header.Set("Connection", "X-Remove")
	req.Header.Set("X-Remove", "private")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "allowed fixture" {
		t.Fatalf("HTTP %d %q", resp.StatusCode, body)
	}
	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = fmt.Fprint(conn, "CONNECT model.example:443 HTTP/1.1\r\nHost: model.example:443\r\n\r\n")
	reader := bufio.NewReader(conn)
	resp, err = http.ReadResponse(reader, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT %v %v", resp, err)
	}
	_, _ = fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: model.example:80\r\nConnection: close\r\n\r\n")
	_ = conn.(*net.TCPConn).CloseWrite()
	resp, err = http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "allowed fixture" || dials.Load() != 2 {
		t.Fatalf("tunnel body=%q dials=%d", body, dials.Load())
	}
}

func TestProxyDeniesUnlistedAndRebindingWithoutDial(t *testing.T) {
	p, _ := New([]string{"model.example:443", "registry.example:80"}, time.Second)
	var dials atomic.Int32
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("unexpected")
	}
	proxy := proxyFixture(t, p)
	parsed, _ := url.Parse(proxy.URL)
	for _, target := range []string{"127.0.0.1:9999", "[::1]:9999", "host.docker.internal:9999", "gateway.docker.internal:9999", "172.17.0.1:9999", "github.com:443", "1.1.1.1:443", "model.example:80", "model.example:444"} {
		conn, err := net.Dial("tcp", parsed.Host)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		_ = conn.Close()
		if err != nil || resp.StatusCode != 403 {
			t.Errorf("target %s: %v %v", target, resp, err)
		}
	}
	for _, address := range []string{"127.0.0.1", "::1", "192.168.65.254", "169.254.169.254", "::ffff:127.0.0.1"} {
		p.lookup = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr(address)}, nil
		}
		if _, err := p.connect(context.Background(), "model.example:443"); err == nil {
			t.Errorf("rebinding accepted: %s", address)
		}
	}
	if dials.Load() != 0 {
		t.Fatalf("forbidden destination reached dial %d times", dials.Load())
	}
}

func TestProxyResolvesAgainAndPinsValidatedIP(t *testing.T) {
	p, _ := New([]string{"model.example:443"}, time.Second)
	lookups, dials := 0, 0
	p.lookup = func(context.Context, string) ([]netip.Addr, error) {
		lookups++
		if lookups == 1 {
			return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	p.dial = func(_ context.Context, _, address string) (net.Conn, error) {
		dials++
		if address != "1.1.1.1:443" {
			t.Fatalf("second DNS lookup possible: %s", address)
		}
		a, b := net.Pipe()
		_ = b.Close()
		return a, nil
	}
	conn, err := p.connect(context.Background(), "model.example:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if _, err := p.connect(context.Background(), "model.example:443"); err == nil {
		t.Fatal("rebinding accepted")
	}
	if lookups != 2 || dials != 1 {
		t.Fatalf("lookups=%d dials=%d", lookups, dials)
	}
}
