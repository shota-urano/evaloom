// Package sandboxproxy implements the trusted network bridge. The workload
// shares ONLY the ingress container's network-none namespace. Egress listens
// on a Unix socket in a volume which is never mounted into the workload.
package sandboxproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	Socket  = "/evaloom-ipc/proxy.sock"
	Address = "127.0.0.1:18080"
	URL     = "http://" + Address
)

// Destination accepts exact DNS names and ports, never wildcards, URLs or IPs.
// DNS resolution is done at each dial by egress, never by the workload.
func Destination(value string) (string, error) {
	host, port, err := net.SplitHostPort(value)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port ||
		host != strings.ToLower(host) || len(host) > 253 || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return "", errors.New("exact DNS host:port required")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid DNS label")
		}
		for _, ch := range label {
			if ch != '-' && (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
				return "", errors.New("invalid DNS character")
			}
		}
	}
	for _, suffix := range []string{"localhost", "local", "internal", "home.arpa"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return "", errors.New("local destination forbidden")
		}
	}
	return value, nil
}

// Reject special-use ranges as well as RFC1918/ULA. In particular Docker
// Desktop's synthetic host addresses, link-local metadata, mapped IPv4,
// NAT64 and transition prefixes must not turn an allowed DNS name into a
// route to the host. No second hostname lookup occurs after this check.
var special = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3",
		"::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16",
		"fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func PublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range special {
		if prefix.Contains(ip) {
			return false
		}
	}
	// Only the current IPv6 global-unicast allocation is eligible.
	return ip.Is4() || netip.MustParsePrefix("2000::/3").Contains(ip)
}

type Proxy struct {
	allowed map[string]bool
	timeout time.Duration
	lookup  func(context.Context, string) ([]netip.Addr, error)
	dial    func(context.Context, string, string) (net.Conn, error)
}

func New(destinations []string, timeout time.Duration) (*Proxy, error) {
	if timeout <= 0 || len(destinations) == 0 {
		return nil, errors.New("explicit destinations and timeout required")
	}
	p := &Proxy{allowed: make(map[string]bool), timeout: timeout,
		dial: (&net.Dialer{Timeout: timeout}).DialContext}
	// LookupNetIP also takes a network argument.
	p.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	for _, value := range destinations {
		if _, err := Destination(value); err != nil {
			return nil, err
		}
		p.allowed[value] = true
	}
	return p, nil
}

func (p *Proxy) connect(ctx context.Context, address string) (net.Conn, error) {
	if !p.allowed[address] {
		return nil, errors.New("destination denied")
	}
	host, port, _ := net.SplitHostPort(address)
	ips, err := p.lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("DNS unavailable")
	}
	for _, ip := range ips {
		if !PublicIP(ip) {
			return nil, errors.New("nonpublic destination denied")
		}
	}
	for _, ip := range ips {
		conn, err := p.dial(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("destination unavailable")
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	deny := func() { http.Error(w, "sandbox destination denied or unavailable", http.StatusForbidden) }
	if r.Method == http.MethodConnect {
		upstream, err := p.connect(ctx, r.Host)
		if err != nil {
			deny()
			return
		}
		defer upstream.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(p.timeout))
		_ = upstream.SetDeadline(time.Now().Add(p.timeout))
		if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err := buffered.Flush(); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, buffered); closeWrite(upstream); close(done) }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-done
		return
	}
	if r.URL.Scheme != "http" || r.URL.User != nil || r.URL.Host == "" || r.Host != r.URL.Host || r.Header.Get("Upgrade") != "" {
		deny()
		return
	}
	address := r.URL.Host
	if r.URL.Port() == "" {
		address = net.JoinHostPort(r.URL.Hostname(), "80")
	}
	if !p.allowed[address] {
		deny()
		return
	}
	transport := &http.Transport{DialContext: p.connectDial, DisableKeepAlives: true, ResponseHeaderTimeout: p.timeout}
	defer transport.CloseIdleConnections()
	req := r.Clone(ctx)
	req.RequestURI = ""
	stripHopHeaders(req.Header)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		deny()
		return
	}
	defer resp.Body.Close()
	stripHopHeaders(resp.Header)
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) connectDial(ctx context.Context, _, address string) (net.Conn, error) {
	return p.connect(ctx, address)
}

func stripHopHeaders(header http.Header) {
	for _, key := range strings.Split(header.Get("Connection"), ",") {
		header.Del(strings.TrimSpace(key))
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(key)
	}
}

// Ingress exposes HTTP proxying on the isolated loopback only. It has no
// upstream IP connectivity and cannot bypass egress's destination checks.
func Ingress(timeout time.Duration) http.Handler {
	return ingress(timeout, Socket)
}

func ingress(timeout time.Duration, socket string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := net.DialTimeout("unix", socket, timeout)
		if err != nil {
			http.Error(w, "sandbox egress unavailable", 503)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(timeout))
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(timeout))
		if err := r.WriteProxy(conn); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { _, _ = io.Copy(conn, buffered); closeWrite(conn); close(done) }()
		_, _ = io.Copy(client, conn)
		_ = client.Close()
		<-done
	})
}

func closeWrite(conn net.Conn) {
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
	}
}
