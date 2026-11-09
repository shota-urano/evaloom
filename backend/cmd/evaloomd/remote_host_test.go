package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the startup flag through a real loopback listener, including the
// default with no remote name. tailscale serve itself is configured by the owner.
func TestRunRemoteHostFlag(t *testing.T) {
	const remoteHost = "orionmac-mini.tailb76d84.ts.net"
	for _, enabled := range []bool{false, true} {
		name := "default"
		if enabled {
			name = "remote-host"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evaloom.db")
			configureTestOwner(t, path)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			args := []string{"-db", path, "-port", "0"}
			if enabled {
				args = append(args, "-remote-host", remoteHost)
			}
			output := make(startupOutput, 1)
			done := make(chan error, 1)
			go func() { done <- run(ctx, args, output) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("run: %v", err)
					}
				case <-time.After(2 * shutdownTimeout):
					t.Error("run did not stop after cancellation")
				}
			})
			var line string
			select {
			case line = <-output:
			case <-ctx.Done():
				t.Fatal("server did not start")
			}
			address, ok := strings.CutPrefix(strings.TrimSpace(line), "evaloomd: listening on ")
			host, _, err := net.SplitHostPort(address)
			if !ok || err != nil || host != "127.0.0.1" {
				t.Fatalf("startup changed listener address: %q err=%v", line, err)
			}
			transport := &http.Transport{}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			for _, remote := range []bool{false, true} {
				r, err := http.NewRequest("POST", "http://"+address+"/auth/login", strings.NewReader(testOwnerLoginBody))
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Origin", "http://"+address)
				if remote {
					r.Host = remoteHost
					r.Header.Set("Origin", "https://"+remoteHost)
				}
				response, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if remote && !enabled {
					if response.StatusCode != http.StatusForbidden || len(response.Cookies()) != 0 {
						t.Fatalf("unconfigured remote login: %d %s", response.StatusCode, body)
					}
					continue
				}
				cookies := response.Cookies()
				if response.StatusCode != http.StatusOK || len(cookies) != 1 || cookies[0].Secure != remote {
					t.Fatalf("login remote=%v: status=%d cookies=%v body=%s", remote, response.StatusCode, cookies, body)
				}
			}
		})
	}
}
