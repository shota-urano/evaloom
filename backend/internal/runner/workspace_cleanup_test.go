package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeWorkspaceFile(t *testing.T, root, id string) string {
	t.Helper()
	path := filepath.Join(root, id, "nested", "file")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("workspace content"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHeartbeatCleansOnlyInstructedWorkspacesAndReportsNextTime(t *testing.T) {
	requests := make(chan heartbeatInput, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- readHeartbeat(t, r)
		io.WriteString(w, `{"cancelExecutionIds":["active"],"cleanWorkspaceIds":["remove","remove","already-absent"]}`)
	}))
	defer server.Close()
	config := testConfig(t, server.URL)
	removed := writeWorkspaceFile(t, config.WorkspaceRoot, "remove")
	kept := writeWorkspaceFile(t, config.WorkspaceRoot, "keep")
	process, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := process.RegisterExecution("active", cancel); err != nil {
		t.Fatal(err)
	}
	if err := process.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first := <-requests; len(first.CleanedWorkspaceIDs) != 0 {
		t.Fatalf("cleanup reported before receiving instructions: %+v", first)
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("cleanup prevented execution cancellation")
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(removed))); !os.IsNotExist(err) {
		t.Fatalf("instructed workspace still exists: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("uninstructed workspace changed: %v", err)
	}
	// Repeated instructions remain harmless and generate one acknowledgement
	// per ID, including a path already absent after a previous process deleted it.
	for range 2 {
		if err := process.Heartbeat(context.Background()); err != nil {
			t.Fatal(err)
		}
		if input := <-requests; !reflect.DeepEqual(input.CleanedWorkspaceIDs, []string{"already-absent", "remove"}) {
			t.Fatalf("incorrect cleanup acknowledgement: %+v", input)
		}
	}
}

func TestHeartbeatRetainsCleanupReportsUntilValidResponse(t *testing.T) {
	for _, failure := range []struct {
		name, body string
		status     int
	}{
		{"unavailable", "unavailable", http.StatusServiceUnavailable},
		{"malformed", `{"cancelExecutionIds":[],`, http.StatusOK},
	} {
		t.Run(failure.name, func(t *testing.T) {
			requests := make(chan heartbeatInput, 4)
			step := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- readHeartbeat(t, r)
				step++
				switch step {
				case 1:
					io.WriteString(w, `{"cancelExecutionIds":[],"cleanWorkspaceIds":["remove"]}`)
				case 2:
					w.WriteHeader(failure.status)
					io.WriteString(w, failure.body)
				default:
					io.WriteString(w, `{"cancelExecutionIds":[],"cleanWorkspaceIds":[]}`)
				}
			}))
			defer server.Close()
			config := testConfig(t, server.URL)
			writeWorkspaceFile(t, config.WorkspaceRoot, "remove")
			process, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			for i := range 4 {
				err := process.Heartbeat(context.Background())
				if (err != nil) != (i == 1) {
					t.Fatalf("step %d: unexpected heartbeat result: %v", i, err)
				}
				input := <-requests
				var want []string
				if i == 1 || i == 2 {
					want = []string{"remove"}
				}
				if !reflect.DeepEqual(input.CleanedWorkspaceIDs, want) {
					t.Fatalf("step %d: cleaned IDs = %v; want %v", i, input.CleanedWorkspaceIDs, want)
				}
			}
		})
	}
}

func TestHeartbeatCleanupRejectsUnsafePathsAndReportsOnlySuccess(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../outside", "nested/../../outside", "/absolute", "back\\slash", "nul\x00byte", "outside-link", "inside-link", "not-a-directory"} {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			requests := make(chan heartbeatInput, 2)
			instructions, err := json.Marshal(map[string]any{"cancelExecutionIds": []string{}, "cleanWorkspaceIds": []string{id, "safe"}})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- readHeartbeat(t, r)
				w.Write(instructions)
			}))
			defer server.Close()
			config := testConfig(t, server.URL)
			outside := writeWorkspaceFile(t, filepath.Dir(config.WorkspaceRoot), "outside")
			inside := writeWorkspaceFile(t, config.WorkspaceRoot, "keep")
			writeWorkspaceFile(t, config.WorkspaceRoot, "safe")
			for name, target := range map[string]string{"outside-link": filepath.Dir(filepath.Dir(outside)), "inside-link": filepath.Join(config.WorkspaceRoot, "keep")} {
				if err := os.Symlink(target, filepath.Join(config.WorkspaceRoot, name)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(config.WorkspaceRoot, "not-a-directory"), []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			process, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				if err := process.Heartbeat(context.Background()); err == nil || strings.Contains(err.Error(), config.WorkspaceRoot) {
					t.Fatalf("unsafe cleanup was accepted or disclosed a local path: %v", err)
				}
				input := <-requests
				if i == 1 && !reflect.DeepEqual(input.CleanedWorkspaceIDs, []string{"safe"}) {
					t.Fatalf("failed cleanup was acknowledged or successful cleanup was lost: %+v", input)
				}
			}
			for _, path := range []string{outside, inside, filepath.Join(config.WorkspaceRoot, "not-a-directory")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("unsafe cleanup changed protected content: %v", err)
				}
			}
		})
	}
}

func TestWorkspaceCleanupDoesNotFollowNestedSymlinks(t *testing.T) {
	config := testConfig(t, "http://127.0.0.1:9999")
	writeWorkspaceFile(t, config.WorkspaceRoot, "remove")
	outside := writeWorkspaceFile(t, filepath.Dir(config.WorkspaceRoot), "outside")
	if err := os.Symlink(filepath.Dir(filepath.Dir(outside)), filepath.Join(config.WorkspaceRoot, "remove", "link")); err != nil {
		t.Fatal(err)
	}
	process, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.cleanWorkspace("remove"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("cleanup followed a nested symlink: %v", err)
	}
}

func TestHeartbeatDoesNotCleanOnFailedResponse(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				body := `{"cancelExecutionIds":[],"cleanWorkspaceIds":["keep"]}`
				if status == http.StatusOK {
					body += ` {}` // A complete-looking response followed by invalid JSON.
				}
				io.WriteString(w, body)
			}))
			defer server.Close()
			config := testConfig(t, server.URL)
			kept := writeWorkspaceFile(t, config.WorkspaceRoot, "keep")
			process, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := process.Heartbeat(context.Background()); err == nil {
				t.Fatal("failed heartbeat response accepted")
			}
			if _, err := os.Stat(kept); err != nil {
				t.Fatalf("failed response deleted a workspace: %v", err)
			}
		})
	}
}
