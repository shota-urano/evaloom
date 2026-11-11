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
	RunnerID              string            `json:"runnerId"`
	ControlPlaneURL       string            `json:"controlPlaneUrl"`
	TokenFile             string            `json:"tokenFile"`
	WorkspaceRoot         string            `json:"workspaceRoot"`
	DataDir               string            `json:"dataDir"`
	SecretsDir            string            `json:"secretsDir"`
	MicroHarnessAuthFiles map[string]string `json:"microHarnessAuthFiles"`
	Container             ContainerConfig   `json:"container"`
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
// for Harness sandboxes. The stage supplies its own timeout
// after applying Project overrides; CPU and memory are always Runner-owned.
func (config ContainerConfig) ResourceLimits(timeout time.Duration) sandbox.ResourceLimits {
	return sandbox.ResourceLimits{CPU: config.CPUs, MemoryBytes: config.MemoryMiB * (1 << 20), Time: timeout}
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
		Container: ContainerConfig{CPUs: 4, MemoryMiB: 8192},
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
		{"tokenFile", config.TokenFile}, {"workspaceRoot", config.WorkspaceRoot}, {"dataDir", config.DataDir}, {"secretsDir", config.SecretsDir},
		{"container.runtimePath", config.Container.RuntimePath}, {"container.proxyBinary", config.Container.ProxyBinary},
	} {
		if !filepath.IsAbs(path.value) || strings.ContainsRune(path.value, 0) {
			return fmt.Errorf("runner: %s must be an absolute path", path.name)
		}
	}
	workspaceRoot, workspaceErr := resolveConfigPath(config.WorkspaceRoot)
	dataDir, dataErr := resolveConfigPath(config.DataDir)
	if workspaceErr != nil || dataErr != nil {
		return fmt.Errorf("runner: cannot resolve workspaceRoot or dataDir")
	}
	if configPathsOverlap(config.WorkspaceRoot, config.DataDir) || configPathsOverlap(workspaceRoot, dataDir) {
		return fmt.Errorf("runner: workspaceRoot and dataDir must not overlap")
	}
	if config.MicroHarnessAuthFiles == nil {
		return fmt.Errorf("runner: microHarnessAuthFiles is required")
	}
	for _, path := range config.MicroHarnessAuthFiles {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) || !configPathWithin(config.DataDir, path) {
			return fmt.Errorf("runner: microHarnessAuthFiles paths must be absolute and inside dataDir")
		}
		// Missing files are handled before the CLI starts (spec 08, 3.4).
		// Check permissions now when a stored authentication file exists.
		info, err := os.Stat(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("runner: cannot inspect microHarnessAuthFiles permissions")
		}
		if err == nil && info.Mode().Perm() != 0600 {
			return fmt.Errorf("runner: microHarnessAuthFiles permissions must be 0600")
		}
	}
	for _, field := range []struct{ name, value string }{
		{"container.image", config.Container.Image}, {"container.proxyImage", config.Container.ProxyImage},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("runner: %s is required", field.name)
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

func configPathsOverlap(first, second string) bool {
	contains := func(root, path string) bool {
		rel, err := filepath.Rel(root, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return contains(first, second) || contains(second, first)
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
// Follow dangling symlinks as well, so containment and existence are separate.
func resolveConfigPath(path string) (string, error) {
	path = strings.TrimRight(path, string(filepath.Separator))
	if path == "" {
		path = string(filepath.Separator)
	}
	real, err := filepath.EvalSymlinks(path)
	if err == nil || !os.IsNotExist(err) {
		return real, err
	}
	if info, linkErr := os.Lstat(path); linkErr == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			parent, _ := filepath.Split(path)
			// Do not clean target yet: resolve links before any '..'.
			target = parent + target
		}
		return resolveConfigPath(target)
	} else if linkErr == nil || !os.IsNotExist(linkErr) {
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
