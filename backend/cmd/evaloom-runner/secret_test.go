package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/runner"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Run the actual main, standard input and exit handling in a child process.
func TestSecretCLIProcess(t *testing.T) {
	if raw := os.Getenv("EVALOOM_TEST_SECRET_ARGS"); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			panic(err)
		}
		os.Args = append([]string{"evaloom-runner"}, args...)
		main()
		return
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	configPath := writeConfig(t, server.URL)
	config, err := runner.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	// No Runner token is needed by the local secret command.
	if err := os.WriteFile(config.TokenFile, []byte("deliberately invalid token"), 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(filepath.Dir(configPath), "management.db")
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	dbBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	values := []string{"first-private-canary", " second-private-canary\nline two\r\n"}
	for i, value := range values {
		access := []string{"read", "write"}[i]
		args := []string{"-config", configPath, "secret", "set", "-access", access, "software-engineer/github_token"}
		output, err := secretProcess(t, args, value)
		if err != nil || bytes.Contains(output, []byte(value)) || bytes.Contains(output, []byte("started")) {
			t.Fatalf("secret CLI failed or exposed value: %v\n%s", err, output)
		}
		path := filepath.Join(config.DataDir, "secrets", "software-engineer", "github_token")
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("CLI did not create 0600 key file")
		}
		content, err := os.ReadFile(path)
		if err != nil || string(content) != access+"\n"+value {
			t.Fatal("CLI did not preserve exact stdin and registered access")
		}
		resolved, err := secretstore.New(config.DataDir).ForStage("software-engineer", model.StagePublish, []contract.Secret{{Slot: "github_token", Access: "write"}})
		if err != nil || resolved.Environment()["EVALOOM_SECRET_GITHUB_TOKEN"] != value {
			t.Fatal("Runner could not read CLI-registered key")
		}
		if i == 1 {
			_, err := secretstore.New(config.DataDir).ForStage("software-engineer", model.StagePrepare, []contract.Secret{{Slot: "github_token", Access: "read"}})
			var configError *secretstore.ConfigError
			if !errors.As(err, &configError) {
				t.Fatal("CLI write registration was downgraded for prepare")
			}
		}
		// All files, including SQLite/WAL, config and possible logs, are under
		// this isolated fixture. Only the one store file may contain the value.
		if err := filepath.WalkDir(filepath.Dir(configPath), func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			content, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			for _, candidate := range values {
				if bytes.Contains(content, []byte(candidate)) && name != path {
					return fmt.Errorf("key found outside store file")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"secret", "set", "software-engineer/github_token"},
		{"secret", "set", "-access", "read", "software-engineer/github_token", "private-argument-canary"},
		{"secret", "set", "-private-argument-canary", "software-engineer/github_token"},
		{"secret", "set", "-access", "private-argument-canary", "software-engineer/github_token"},
		{"secret", "set", "-access", "read", "../github_token"},
	} {
		output, err := secretProcess(t, append([]string{"-config", configPath}, args...), "private-stdin-canary")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || bytes.Contains(output, []byte("private-argument-canary")) || bytes.Contains(output, []byte("private-stdin-canary")) {
			t.Fatalf("invalid CLI did not fail privately: %v\n%s", err, output)
		}
	}
	dbAfter, err := os.ReadFile(dbPath)
	if err != nil || !bytes.Equal(dbBefore, dbAfter) || requests.Load() != 0 {
		t.Fatal("secret CLI modified management DB or contacted Control Plane")
	}
}

func secretProcess(t *testing.T, args []string, input string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSecretCLIProcess$")
	cmd.Env = append(os.Environ(), "EVALOOM_TEST_SECRET_ARGS="+string(raw))
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("secret command hung")
	}
	return output, err
}

type failingSecretReader struct{}

func (failingSecretReader) Read(p []byte) (int, error) {
	return copy(p, "private-read-canary"), fmt.Errorf("private-reader-error-canary")
}

func TestSecretInputFailureDoesNotChangeKey(t *testing.T) {
	dir := t.TempDir()
	s := secretstore.New(dir)
	if err := s.Set("test", "key", "read", []byte("original")); err != nil {
		t.Fatal(err)
	}
	err := setSecret(dir, []string{"set", "-access", "write", "test/key"}, failingSecretReader{}, io.Discard)
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("read failure did not fail privately")
	}
	resolved, err := s.ForStage("test", model.StagePrepare, []contract.Secret{{Slot: "key", Access: "read"}})
	if err != nil || resolved.Environment()["EVALOOM_SECRET_KEY"] != "original" {
		t.Fatal("read failure changed existing key")
	}
}
