package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
	for _, content := range []string{"", "null", "[]", "{}", `{"unknown":true}`, string(content) + `{}`, string(content) + "garbage",
		strings.Replace(string(content), `"container":{`, `"container":{"secret-unknown":true},"container":{`, 1),
	} {
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
	valid.TokenFile += ".secret-missing"
	if _, err := New(valid); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("missing token file accepted or path echoed: %v", err)
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

func TestConfigTimeoutDefaultsAndOverrides(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value any
		want  DefaultTimeouts
	}{
		{"omitted", nil, DefaultTimeouts{900, 3600, 1800}},
		{"empty", map[string]any{}, DefaultTimeouts{900, 3600, 1800}},
		{"partial", map[string]any{"harnessSec": 42}, DefaultTimeouts{900, 42, 1800}},
		{"explicit", map[string]any{"setupSec": 1, "harnessSec": 2, "testSec": 3}, DefaultTimeouts{1, 2, 3}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := configDocument(t)
			delete(config, "defaultTimeouts")
			if tt.value != nil {
				config["defaultTimeouts"] = tt.value
			}
			got, err := LoadConfig(writeConfigDocument(t, config))
			if err != nil {
				t.Fatal(err)
			}
			if got.DefaultTimeouts != tt.want {
				t.Fatalf("timeouts=%+v, want %+v", got.DefaultTimeouts, tt.want)
			}
		})
	}
}

func TestConfigRejectsInvalidTypesAndValues(t *testing.T) {
	for i, tt := range []struct {
		key   string
		value any
	}{
		{"harnessAuthFiles", nil}, {"harnessAuthFiles", []string{}},
		{"harnessAuthFiles", map[string]any{"codex": nil}}, {"harnessAuthFiles", map[string]any{"codex": 123}},
		{"container", nil}, {"commitAuthor", nil}, {"defaultTimeouts", nil},
		{"container.image", ""}, {"container.proxyImage", " "},
		{"container.runtimeEnv", nil}, {"container.runtimeEnv", map[string]any{"DOCKER_HOST": false}},
		{"commitAuthor.name", ""}, {"commitAuthor.email", ""},
		{"container.runtimePath", "/secret\x00path"},
		{"githubAppPrivateKeyFile", "/secret/outside.pem"},
		{"defaultTimeouts.setupSec", 0}, {"defaultTimeouts.harnessSec", -1},
		{"defaultTimeouts.testSec", 1.5}, {"defaultTimeouts.setupSec", "secret"},
		{"defaultTimeouts.harnessSec", nil}, {"defaultTimeouts.testSec", int64(1<<63 - 1)},
	} {
		t.Run(tt.key+"/"+strconv.Itoa(i), func(t *testing.T) {
			config := configDocument(t)
			parts := strings.Split(tt.key, ".")
			object := config
			if len(parts) == 2 {
				object = config[parts[0]].(map[string]any)
			}
			object[parts[len(parts)-1]] = tt.value
			if _, err := LoadConfig(writeConfigDocument(t, config)); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid configuration accepted or echoed: %v", err)
			}
		})
	}
}

func TestConfigAuthPathContainmentAndPermissions(t *testing.T) {
	for _, kind := range []string{"outside", "sibling-prefix", "traversal", "root", "symlink-file", "symlink-directory", "symlink-parent-traversal", "symlink-parent-traversal-missing", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			config := testConfig(t, "https://control.example.test")
			if err := os.Mkdir(config.DataDir, 0700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(filepath.Dir(config.DataDir), "secret-auth.json")
			if err := os.WriteFile(outside, []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			path := outside
			switch kind {
			case "sibling-prefix":
				path = config.DataDir + "-other/auth.json"
			case "traversal":
				path = config.DataDir + "/../secret-auth.json"
			case "root":
				path = config.DataDir
			case "symlink-file":
				path = filepath.Join(config.DataDir, "auth.json")
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "symlink-directory":
				link := filepath.Join(config.DataDir, "linked")
				if err := os.Symlink(filepath.Dir(outside), link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "not-created", "auth.json")
			case "symlink-parent-traversal", "symlink-parent-traversal-missing":
				link := filepath.Join(config.DataDir, "linked")
				target := filepath.Join(filepath.Dir(config.DataDir), "outside", "child")
				if err := os.MkdirAll(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink-parent-traversal" {
					if err := os.WriteFile(filepath.Join(filepath.Dir(target), "auth.json"), []byte(`{}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				path = link + "/../auth.json"
			case "permissions":
				path = filepath.Join(config.DataDir, "auth.json")
				if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
					t.Fatal(err)
				}
			}
			config.HarnessAuthFiles["secret-harness"] = path
			if err := config.validate(); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid authentication path accepted or echoed: %v", err)
			}
		})
	}
	t.Run("valid existing file", func(t *testing.T) {
		config := testConfig(t, "https://control.example.test")
		if err := os.Mkdir(config.DataDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config.HarnessAuthFiles["codex"], []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := config.validate(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestConfigMapEntriesAreNotFixedKeys(t *testing.T) {
	for _, empty := range []bool{true, false} {
		config := configDocument(t)
		auth := map[string]string{}
		if !empty {
			auth["another-harness"] = filepath.Join(config["dataDir"].(string), "another", "auth.json")
		}
		config["harnessAuthFiles"] = auth
		env := map[string]string{"DOCKER_HOST": "unix:///runtime/docker.sock"}
		config["container"].(map[string]any)["runtimeEnv"] = env
		got, err := LoadConfig(writeConfigDocument(t, config))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.HarnessAuthFiles, auth) || !reflect.DeepEqual(got.Container.RuntimeEnv, env) {
			t.Fatal("map entries changed while loading configuration")
		}
	}
}

func configDocument(t *testing.T) map[string]any {
	t.Helper()
	content, err := json.Marshal(testConfig(t, "https://control.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(content, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func writeConfigDocument(t *testing.T, config map[string]any) string {
	t.Helper()
	content, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runner.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
