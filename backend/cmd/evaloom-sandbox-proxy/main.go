// evaloom-sandbox-proxy is a trusted sidecar, never a workload command.
// Build statically for the Docker daemon's Linux architecture (CGO_ENABLED=0).
package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/sandboxproxy"
)

func run(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("mode and explicit timeout required")
	}
	timeout, err := time.ParseDuration(args[1])
	if err != nil || timeout <= 0 {
		return fmt.Errorf("invalid timeout")
	}
	if args[0] == "check" {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			conn, err := net.DialTimeout("tcp", sandboxproxy.Address, time.Until(deadline))
			if err == nil {
				_ = conn.SetDeadline(deadline)
				_, err = fmt.Fprint(conn, "CONNECT denied.invalid:443 HTTP/1.1\r\nHost: denied.invalid:443\r\n\r\n")
				if err == nil {
					resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
					if err == nil && resp.StatusCode == http.StatusForbidden {
						_ = conn.Close()
						return nil
					}
				}
				_ = conn.Close()
			}
			time.Sleep(10 * time.Millisecond)
		}
		return fmt.Errorf("proxy path unconfirmed")
	}
	var listener net.Listener
	var handler http.Handler
	switch args[0] {
	case "egress":
		handler, err = sandboxproxy.New(args[2:], timeout)
		if err != nil {
			return err
		}
		listener, err = net.Listen("unix", sandboxproxy.Socket)
	case "ingress":
		handler = sandboxproxy.Ingress(timeout)
		listener, err = net.Listen("tcp", sandboxproxy.Address)
	default:
		return fmt.Errorf("unknown mode")
	}
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: timeout, ReadTimeout: timeout, WriteTimeout: timeout, IdleTimeout: timeout, MaxHeaderBytes: 32 * 1024}
	return server.Serve(listener)
}

func main() {
	args := os.Args[1:]
	os.Clearenv()
	if err := run(args); err != nil {
		// Never emit URLs, runtime diagnostics, request headers or credentials.
		fmt.Fprintln(os.Stderr, "sandbox proxy failed")
		os.Exit(1)
	}
}
