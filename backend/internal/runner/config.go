// Package runner implements the Runner side of the Control Plane protocol.
package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Config struct {
	RunnerID        string `json:"runnerId"`
	ControlPlaneURL string `json:"controlPlaneUrl"`
	TokenFile       string `json:"tokenFile"`
	WorktreeRoot    string `json:"worktreeRoot"`
	DataDir         string `json:"dataDir"`
}

// LoadConfig reads one JSON object. Paths are absolute, independent of the
// working directory chosen by launchd or an interactive shell.
func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("runner: open config: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		// Decoder errors can include input values; never echo config contents.
		return Config{}, fmt.Errorf("runner: invalid config JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, fmt.Errorf("runner: config must contain one JSON object")
	}
	if err := config.validate(); err != nil {
		return Config{}, err
	}
	return config, nil
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
	} {
		if !filepath.IsAbs(path.value) || strings.ContainsRune(path.value, 0) {
			return fmt.Errorf("runner: %s must be an absolute path", path.name)
		}
	}
	return nil
}
