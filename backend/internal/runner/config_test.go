package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	config := testConfig(t, "https://control.example.test")
	content, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runner.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil || !reflect.DeepEqual(got, config) {
		t.Fatalf("config=%+v err=%v", got, err)
	}
	if _, err := LoadConfig(path + ".missing"); err == nil {
		t.Fatal("missing configuration accepted")
	}
	for _, content := range []string{"", "null", "[]", "{}", `{"unknown":true}`, string(content) + `{}`, string(content) + "garbage"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("invalid configuration accepted: %q", content)
		}
	}
}

func TestNewRejectsInvalidConfigAndToken(t *testing.T) {
	valid := testConfig(t, "http://127.0.0.1:8080")
	for _, change := range []func(*Config){
		func(c *Config) { c.RunnerID = "" },
		func(c *Config) { c.RunnerID = ".." },
		func(c *Config) { c.RunnerID = "other/runner" },
		func(c *Config) { c.RunnerID = "white space" },
		func(c *Config) { c.ControlPlaneURL = "/relative" },
		func(c *Config) { c.ControlPlaneURL = "file:///tmp/api" },
		func(c *Config) { c.ControlPlaneURL = "http://" },
		func(c *Config) { c.ControlPlaneURL = "http://user:secret@localhost" },
		func(c *Config) { c.ControlPlaneURL += "?token=secret" },
		func(c *Config) { c.ControlPlaneURL += "#fragment" },
		func(c *Config) { c.TokenFile = "relative" },
		func(c *Config) { c.WorktreeRoot = "" },
		func(c *Config) { c.DataDir = "relative" },
	} {
		config := valid
		change(&config)
		if _, err := New(config); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid configuration accepted or echoed: %v", err)
		}
	}
	valid.TokenFile += ".missing"
	if _, err := New(valid); err == nil {
		t.Fatal("missing token file accepted")
	}
	for _, token := range []string{"", "secret", testToken + "=", testToken + "\n" + testToken} {
		config := testConfig(t, "http://127.0.0.1:8080")
		if err := os.WriteFile(config.TokenFile, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(config); err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("invalid token accepted or echoed: %v", err)
		}
	}
}
