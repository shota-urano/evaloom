package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Spec 18 3.6: neither startup output nor daemon logs contain a login URL,
// whether or not credentials have been configured. The legacy GET must not
// bootstrap an owner session, including when the password is not set yet.
func TestRunDoesNotIssueLoginURL(t *testing.T) {
	for _, configured := range []bool{false, true} {
		name := "password not set"
		if configured {
			name = "password set with CLI"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evaloom.db")
			if configured {
				configureTestOwner(t, path)
			}
			output := &lockedBuffer{}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			done := make(chan error, 1)
			go func() { done <- run(ctx, []string{"-db", path, "-port", "0"}, output) }()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Errorf("run: %v", err)
				}
				for _, forbidden := range []string{"login URL", "/auth/login", "?token=", testOwnerPassword, "$argon2"} {
					if strings.Contains(output.String(), forbidden) {
						t.Errorf("daemon output/logs contain %q: %s", forbidden, output.String())
					}
				}
			})
			var address string
			for address == "" {
				for _, line := range strings.Split(output.String(), "\n") {
					if value, ok := strings.CutPrefix(line, "evaloomd: listening on "); ok {
						address = value
					}
				}
				if address != "" {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("daemon did not start")
				case <-time.After(10 * time.Millisecond):
				}
			}
			response, body := ownerHTTP(t, "GET", address, "/auth/login?token=legacy-canary", "", nil)
			if response.StatusCode != 404 || len(response.Cookies()) != 0 || response.Header.Get("Location") != "" {
				t.Fatalf("legacy login: %d %s %v", response.StatusCode, body, response.Header)
			}
			if response, body := ownerHTTP(t, "GET", address, "/api/session", "", nil); response.StatusCode != 401 {
				t.Fatalf("legacy login authenticated the client: %d %s", response.StatusCode, body)
			}
		})
	}
}
