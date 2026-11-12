package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/logredact"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

// This capability belongs to one invocation, never to the reusable Runner or
// Sandbox. The executor must stop the sandbox and all collectors before return.
type stageSecurity struct {
	mu           sync.Mutex
	runner       *Runner
	id           string
	resolved     secretstore.Resolved
	active       bool
	environments []map[string]string
	logs         []*Log
}

func (runner *Runner) runStageWithSecrets(ctx context.Context, id, harness string, in StageInvocation, executor StageExecutor) StageOutcome {
	var declarations []contract.Secret
	switch in.Stage {
	case model.StagePrepare:
		declarations = in.Profile.Definition.Stages.Prepare.Secrets
	case model.StageCheck:
		declarations = in.Profile.Definition.Stages.Check.Secrets
	case model.StagePublish:
		declarations = in.Profile.Definition.Stages.Publish.Secrets
	}
	var outcome StageOutcome
	err := runner.WithStageSecrets(ctx, harness, in.Stage, declarations, func(ctx context.Context, keys secretstore.Resolved) error {
		security := &stageSecurity{runner: runner, id: id, resolved: keys, active: true}
		in.security = security
		defer func() {
			security.mu.Lock()
			defer security.mu.Unlock()
			security.active = false
			security.resolved = secretstore.Resolved{}
			for _, environment := range security.environments {
				clear(environment)
			}
			security.environments = nil
			for _, log := range security.logs {
				log.mu.Lock()
				clear(log.secrets)
				log.secrets = nil
				log.mu.Unlock()
			}
			security.logs = nil
		}()
		outcome = runner.runStageWithChannel(ctx, id, in, executor)
		// Collectors use CollectArtifact before persistence/registration. Also
		// exclude from retained Sandbox output before it can become later inputs.
		if err := in.redactStageFiles(); err != nil {
			return err
		}
		outcome.Result = in.redactBytes(outcome.Result)
		if outcome.Work != nil {
			work := *outcome.Work
			work.Question = in.redactBytes(work.Question)
			outcome.Work = &work
		}
		return nil
	})
	if err != nil {
		code := "config_error"
		if ctx.Err() != nil {
			code = stageInterruption(ctx.Err())
		}
		return StageOutcome{RunnerError: &code}
	}
	return outcome
}

// SandboxSpec supplies only this invocation's declared keys through the spec 09
// transport. Ordinary Env entries cannot grant keys. Values never enter argv.
// A retained invocation loses its capability as soon as RunStage returns.
func (in StageInvocation) SandboxSpec(spec sandbox.SandboxSpec) (sandbox.SandboxSpec, error) {
	if in.security == nil {
		return sandbox.SandboxSpec{}, fmt.Errorf("runner: stage security is unavailable")
	}
	s := in.security
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || spec.Kind != sandbox.SandboxKind(in.Stage) {
		return sandbox.SandboxSpec{}, fmt.Errorf("runner: stage security is unavailable")
	}
	spec.Secrets = make(map[string]string)
	for key, value := range s.resolved.Environment() {
		spec.Secrets[strings.ToLower(strings.TrimPrefix(key, "EVALOOM_SECRET_"))] = value
	}
	s.environments = append(s.environments, spec.Secrets)
	return spec, nil
}

// NewLog connects the selected keys to the existing streaming exclusion, local
// log, events and log-artifact delivery. Model credentials may be added by the
// Adapter collector. A nil registrar selects Runner's existing HTTP uploader.
func (in StageInvocation) NewLog(registrar ArtifactRegistrar, credentials ...string) (*Log, error) {
	if in.security == nil {
		return nil, fmt.Errorf("runner: stage security is unavailable")
	}
	s := in.security
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return nil, fmt.Errorf("runner: stage security is unavailable")
	}
	log, err := s.runner.NewLog(s.id, string(in.Stage), registrar, append(s.resolved.RedactionValues(), credentials...)...)
	if err == nil {
		s.logs = append(s.logs, log)
	}
	return log, err
}

// CollectArtifact accepts Sandbox output as a reader, excludes secrets in
// memory, and creates only the sanitized copy. The existing artifact delivery
// boundary owns registration/retries; it must use this copy and metadata.
func (in StageInvocation) CollectArtifact(kind, name string, source io.Reader) (LogArtifact, error) {
	if in.security == nil {
		return LogArtifact{}, fmt.Errorf("runner: stage security is unavailable")
	}
	s := in.security
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return LogArtifact{}, fmt.Errorf("runner: stage security is unavailable")
	}
	raw, err := io.ReadAll(source)
	if err != nil {
		return LogArtifact{}, fmt.Errorf("runner: read stage artifact failed")
	}
	data := redactStageBytes(raw, append([]string{s.runner.token}, s.resolved.RedactionValues()...))
	dir := filepath.Join(s.runner.dataDir, "artifacts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return LogArtifact{}, fmt.Errorf("runner: create stage artifact directory failed")
	}
	id := ulid.Make().String()
	path := filepath.Join(dir, id)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return LogArtifact{}, fmt.Errorf("runner: create stage artifact failed")
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return LogArtifact{}, fmt.Errorf("runner: save stage artifact failed")
	}
	return LogArtifact{ID: id, ExecutionID: s.id, Stage: string(in.Stage), Kind: kind, Name: name, Path: path,
		Redacted: !bytes.Equal(raw, data) || bytes.Contains(data, []byte(logredact.Marker))}, nil
}

func (in StageInvocation) redactBytes(raw []byte) []byte {
	return redactStageBytes(raw, append([]string{in.security.runner.token}, in.security.resolved.RedactionValues()...))
}

func secretLiterals(values []string) []string {
	// Match both a credential's original bytes and its JSON string spelling.
	// The latter can occur in non-JSON text or inside another JSON string.
	literals := append([]string(nil), values...)
	for _, value := range values {
		encoded, _ := json.Marshal(value)
		literals = append(literals, string(encoded[1:len(encoded)-1]))
		var plain bytes.Buffer
		encoder := json.NewEncoder(&plain)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(value) // Encoding a string into a bytes.Buffer cannot fail.
		encoded = bytes.TrimSuffix(plain.Bytes(), []byte("\n"))
		literals = append(literals, string(encoded[1:len(encoded)-1]))
	}
	return literals
}

// Inputs come from UseNumber decoders of valid JSON. Compare decimal values
// exactly, without float rounding or expanding potentially enormous exponents.
func canonicalJSONNumber(number json.Number) string {
	digits := string(number)
	negative := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")
	var exponent big.Int
	if i := strings.IndexAny(digits, "eE"); i >= 0 {
		exponent.SetString(digits[i+1:], 10)
		digits = digits[:i]
	}
	fraction := 0
	if i := strings.IndexByte(digits, '.'); i >= 0 {
		fraction = len(digits) - i - 1
		digits = digits[:i] + digits[i+1:]
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0"
	}
	trimmed := strings.TrimRight(digits, "0")
	shift := int64(len(digits) - len(trimmed) - fraction)
	exponent.Add(&exponent, big.NewInt(shift))
	if negative {
		trimmed = "-" + trimmed
	}
	return trimmed + "e" + exponent.String()
}

func equalJSONValues(left, right any) bool {
	switch left := left.(type) {
	case json.Number:
		right, ok := right.(json.Number)
		return ok && canonicalJSONNumber(left) == canonicalJSONNumber(right)
	case map[string]any:
		right, ok := right.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for key, value := range left {
			other, ok := right[key]
			if !ok || !equalJSONValues(value, other) {
				return false
			}
		}
		return true
	case []any:
		right, ok := right.([]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for i, value := range left {
			if !equalJSONValues(value, right[i]) {
				return false
			}
		}
		return true
	default: // JSON's remaining scalar types are comparable: string, bool, nil.
		return left == right
	}
}

func redactStageBytes(raw []byte, values []string) []byte {
	if len(raw) == 0 {
		return raw
	}
	literals := secretLiterals(values)
	redact := func(raw []byte) []byte {
		return logredact.Bytes(raw, literals)
	}
	if !json.Valid(raw) {
		return redact(raw)
	}
	// Q203 also matches credentials that are themselves JSON objects, arrays or
	// numbers. Decode without float64 so adjacent large numbers stay distinct.
	var structured []any
	for _, value := range values {
		var decoded any
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.UseNumber()
		if json.Valid([]byte(value)) && decoder.Decode(&decoded) == nil {
			switch decoded.(type) {
			case map[string]any, []any, json.Number:
				structured = append(structured, decoded)
			}
		}
	}
	// Record token spans rather than serializing a decoded map: unrelated
	// formatting, number spellings and duplicate object keys must survive.
	// Patterns apply only to decoded strings, never to JSON syntax. Match whole
	// values against the original tree before any child replacement takes effect.
	type replacement struct {
		start, end int
		data       []byte
	}
	var replacements []replacement
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var visit func() (any, error)
	visit = func() (any, error) {
		start := int(decoder.InputOffset())
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		for strings.ContainsRune(" \t\r\n,:", rune(raw[start])) {
			start++
		}
		before := len(replacements)
		var original any = token
		switch token := token.(type) {
		case string:
			clean := redact([]byte(token))
			if !bytes.Equal(clean, []byte(token)) {
				encoded, _ := json.Marshal(string(clean))
				replacements = append(replacements, replacement{start, int(decoder.InputOffset()), encoded})
			}
		case json.Delim:
			if token == '{' {
				object := make(map[string]any)
				for decoder.More() {
					key, err := visit()
					if err != nil {
						return nil, err
					}
					value, err := visit()
					if err != nil {
						return nil, err
					}
					object[key.(string)] = value
				}
				original = object
			} else {
				array := make([]any, 0)
				for decoder.More() {
					value, err := visit()
					if err != nil {
						return nil, err
					}
					array = append(array, value)
				}
				original = array
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
		}
		for _, secret := range structured {
			if equalJSONValues(original, secret) {
				replacements = append(replacements[:before], replacement{start, int(decoder.InputOffset()), []byte(`"[REDACTED]"`)})
				break
			}
		}
		return original, nil
	}
	if _, err := visit(); err != nil || len(replacements) == 0 {
		return raw
	}
	var output []byte
	last := 0
	for _, replacement := range replacements {
		output = append(output, raw[last:replacement.start]...)
		output = append(output, replacement.data...)
		last = replacement.end
	}
	return append(output, raw[last:]...)
}

func (in StageInvocation) redactStageFiles() error {
	root, err := os.OpenRoot(in.Directory)
	if err != nil {
		return err
	}
	defer root.Close()
	if info, err := root.Lstat("out"); err != nil || !info.IsDir() {
		return fmt.Errorf("runner: unsafe stage output directory")
	}
	clean := func(name string) error {
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("runner: unsafe stage output")
		}
		// Reject hardlinks. Do not overwrite
		// another region through a file supplied by the stage.
		file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err = file.Stat()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 {
			return fmt.Errorf("runner: unsafe stage output")
		}
		raw, err := io.ReadAll(file)
		if err != nil {
			return err
		}
		data := in.redactBytes(raw)
		if bytes.Equal(raw, data) {
			return nil
		}
		// Replace the Sandbox file atomically, including read-only files. Only
		// sanitized bytes ever enter the temporary Runner-owned file.
		tmp := filepath.Join(filepath.Dir(name), ".redacted-"+ulid.Make().String())
		output, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer root.Remove(tmp)
		_, writeErr := output.Write(data)
		closeErr := output.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		return root.Rename(tmp, name)
	}
	err = fs.WalkDir(root.FS(), "out", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		return clean(name)
	})
	if err != nil {
		return err
	}
	if _, err := root.Lstat("result.json"); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return clean("result.json")
}
