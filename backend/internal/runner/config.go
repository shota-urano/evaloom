// Package runner implements the Runner side of the Control Plane protocol.
package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

type Config struct {
	RunnerID                string             `json:"runnerId"`
	ControlPlaneURL         string             `json:"controlPlaneUrl"`
	TokenFile               string             `json:"tokenFile"`
	WorktreeRoot            string             `json:"worktreeRoot"`
	DataDir                 string             `json:"dataDir"`
	HarnessAuthFiles        map[string]string  `json:"harnessAuthFiles"`
	GitHubAppPrivateKeyFile string             `json:"githubAppPrivateKeyFile"`
	Container               ContainerConfig    `json:"container"`
	CommitAuthor            CommitAuthorConfig `json:"commitAuthor"`
	DefaultTimeouts         DefaultTimeouts    `json:"defaultTimeouts"`
}

type ContainerConfig struct {
	RuntimePath string            `json:"runtimePath"`
	Image       string            `json:"image"`
	ProxyImage  string            `json:"proxyImage"`
	ProxyBinary string            `json:"proxyBinary"`
	RuntimeEnv  map[string]string `json:"runtimeEnv,omitempty"`
	CPUs        float64           `json:"cpus"`
	MemoryMiB   int64             `json:"memoryMiB"`
}

// ResourceLimits converts validated Runner configuration to the common limits
// for Harness, setup and test sandboxes. The stage supplies its own timeout
// after applying Project overrides; CPU and memory are always Runner-owned.
func (config ContainerConfig) ResourceLimits(timeout time.Duration) sandbox.ResourceLimits {
	return sandbox.ResourceLimits{CPU: config.CPUs, MemoryBytes: config.MemoryMiB * (1 << 20), Time: timeout}
}

type CommitAuthorConfig struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// DefaultTimeouts stores seconds as specified by the configuration file, not
// time.Duration's nanoseconds. Wiring these limits into stages is separate.
type DefaultTimeouts struct {
	SetupSec   int `json:"setupSec"`
	HarnessSec int `json:"harnessSec"`
	TestSec    int `json:"testSec"`
}

// LoadConfig reads one JSON object. Paths are absolute, independent of the
// working directory chosen by launchd or an interactive shell.
func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("runner: cannot open config")
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		// Decoder errors can include input values; never echo config contents.
		return Config{}, fmt.Errorf("runner: invalid config JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, fmt.Errorf("runner: config must contain one JSON object")
	}
	// encoding/json accepts case-insensitive field aliases and null scalars.
	// Check the exact schema first so neither can bypass the table in spec 05.
	if !validConfigJSON(raw, reflect.TypeOf(Config{})) {
		return Config{}, fmt.Errorf("runner: invalid config JSON")
	}
	config := Config{
		Container:       ContainerConfig{CPUs: 4, MemoryMiB: 8192},
		DefaultTimeouts: DefaultTimeouts{SetupSec: 900, HarnessSec: 3600, TestSec: 1800},
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return Config{}, fmt.Errorf("runner: invalid config JSON")
	}
	if err := config.validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func validConfigJSON(raw json.RawMessage, typ reflect.Type) bool {
	if strings.TrimSpace(string(raw)) == "null" {
		return false
	}
	if typ.Kind() != reflect.Struct && typ.Kind() != reflect.Map {
		return true // Unmarshal checks scalar types without echoing their values.
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return false
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
		if typ.Kind() == reflect.Map {
			if !validConfigJSON(value, typ.Elem()) {
				return false
			}
			continue
		}
		var fieldType reflect.Type
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if strings.Split(field.Tag.Get("json"), ",")[0] == key {
				fieldType = field.Type
				break
			}
		}
		if fieldType == nil || !validConfigJSON(value, fieldType) {
			return false
		}
	}
	token, err := decoder.Token()
	return err == nil && token == json.Delim('}')
}

func (config Config) validate() error {
	// Match the ID constraints of runnerauth.ValidateRegistration without
	// depending on the Control Plane's database packages.
	id := config.RunnerID
	if id == "" || id == "." || id == ".." || !utf8.ValidString(id) || strings.ContainsAny(id, `/\`) || strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("runner: runnerId must be a URL path segment without whitespace or control characters")
	}
	u, err := url.Parse(config.ControlPlaneURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("runner: controlPlaneUrl must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	for _, path := range []struct{ name, value string }{
		{"tokenFile", config.TokenFile}, {"worktreeRoot", config.WorktreeRoot}, {"dataDir", config.DataDir},
		{"githubAppPrivateKeyFile", config.GitHubAppPrivateKeyFile},
		{"container.runtimePath", config.Container.RuntimePath}, {"container.proxyBinary", config.Container.ProxyBinary},
	} {
		if !filepath.IsAbs(path.value) || strings.ContainsRune(path.value, 0) {
			return fmt.Errorf("runner: %s must be an absolute path", path.name)
		}
	}
	if config.HarnessAuthFiles == nil {
		return fmt.Errorf("runner: harnessAuthFiles is required")
	}
	for _, path := range config.HarnessAuthFiles {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) || !configPathWithin(config.DataDir, path) {
			return fmt.Errorf("runner: harnessAuthFiles paths must be absolute and inside dataDir")
		}
		// Missing files are handled before the CLI starts (spec 08, 3.4).
		// Check permissions now when a stored authentication file exists.
		info, err := os.Stat(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("runner: cannot inspect harnessAuthFiles permissions")
		}
		if err == nil && info.Mode().Perm() != 0600 {
			return fmt.Errorf("runner: harnessAuthFiles permissions must be 0600")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) || !configPathWithin(filepath.Join(home, ".config", "evaloom"), config.GitHubAppPrivateKeyFile) {
		return fmt.Errorf("runner: githubAppPrivateKeyFile must be inside the host's ~/.config/evaloom directory")
	}
	for _, field := range []struct{ name, value string }{
		{"container.image", config.Container.Image}, {"container.proxyImage", config.Container.ProxyImage},
		{"commitAuthor.name", config.CommitAuthor.Name}, {"commitAuthor.email", config.CommitAuthor.Email},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("runner: %s is required", field.name)
		}
	}
	for _, field := range []struct {
		name    string
		seconds int
	}{
		{"defaultTimeouts.setupSec", config.DefaultTimeouts.SetupSec},
		{"defaultTimeouts.harnessSec", config.DefaultTimeouts.HarnessSec},
		{"defaultTimeouts.testSec", config.DefaultTimeouts.TestSec},
	} {
		if field.seconds <= 0 || uint64(field.seconds) > uint64((1<<63-1)/time.Second) {
			return fmt.Errorf("runner: %s must be positive seconds within the supported duration range", field.name)
		}
	}
	if config.Container.CPUs <= 0 || math.IsNaN(config.Container.CPUs) || math.IsInf(config.Container.CPUs, 0) {
		return fmt.Errorf("runner: container.cpus must be a finite positive number")
	}
	if config.Container.MemoryMiB <= 0 || config.Container.MemoryMiB > math.MaxInt64/(1<<20) {
		return fmt.Errorf("runner: container.memoryMiB must be positive MiB within the supported byte range")
	}
	return nil
}

func configPathWithin(root, path string) bool {
	inside := func(root, path string) bool {
		rel, err := filepath.Rel(root, path)
		return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if !inside(root, path) {
		return false
	}
	resolvedRoot, rootErr := resolveConfigPath(root)
	resolvedPath, pathErr := resolveConfigPath(path)
	return rootErr == nil && pathErr == nil && inside(resolvedRoot, resolvedPath)
}

// Resolve existing ancestors too: configuration may name files/directories
// that will only be created later, but an existing symlink must not escape.
func resolveConfigPath(path string) (string, error) {
	path = strings.TrimRight(path, string(filepath.Separator))
	if path == "" {
		path = string(filepath.Separator)
	}
	real, err := filepath.EvalSymlinks(path)
	if err == nil || !os.IsNotExist(err) {
		return real, err
	}
	if _, linkErr := os.Lstat(path); linkErr == nil || !os.IsNotExist(linkErr) {
		return "", err
	}
	// Split before cleaning: link/../file must resolve link before '..'.
	parent, base := filepath.Split(path)
	if base == "" || parent == path {
		return "", err
	}
	realParent, err := resolveConfigPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, base), nil
}
