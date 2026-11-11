package secretstore

import (
	"fmt"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// ConfigError is safe to include in a result report. It never contains key
// values, paths, input identifiers or decoder errors.
type ConfigError struct{}

func (*ConfigError) Error() string {
	return "secretstore: required stage keys are missing, unsafe or incompatible"
}
func (*ConfigError) Status() model.ExecutionStatus { return model.ExecutionFailed }
func (*ConfigError) ErrorClass() model.ErrorClass  { return model.ErrorConfig }

// Resolved holds only the declared stage's keys. Callers supply Environment to
// the stage transport and RedactionValues to log/artifact exclusion. Do not
// serialize these values or put them in process arguments or request JSON.
type Resolved struct{ env map[string]string }

func (Resolved) String() string   { return "[stage secrets]" }
func (Resolved) GoString() string { return "[stage secrets]" }

func (r Resolved) Environment() map[string]string {
	env := make(map[string]string, len(r.env))
	for name, value := range r.env {
		env[name] = value
	}
	return env
}

func (r Resolved) RedactionValues() []string {
	values := make([]string, 0, len(r.env))
	for _, value := range r.env {
		values = append(values, value)
	}
	return values
}

// ForStage resolves all declarations before returning any values. Stored write
// keys cannot be downgraded by a prepare declaration. Publish may use either
// stored access, as specified in 01 §3.5 and 05 §3.6.
func (s *Store) ForStage(harnessID string, stage model.Stage, declarations []contract.Secret) (Resolved, error) {
	fail := func() (Resolved, error) { return Resolved{}, &ConfigError{} }
	switch stage {
	case model.StagePrepare, model.StagePublish:
	case model.StageWork, model.StageCheck, model.Stage("proposal"):
		if len(declarations) > 0 {
			return fail()
		}
		return Resolved{}, nil
	default:
		return fail()
	}
	if !harnessPattern.MatchString(harnessID) {
		return fail()
	}
	env := make(map[string]string, len(declarations))
	for _, declaration := range declarations {
		if Validate(harnessID, declaration.Slot, declaration.Access) != nil {
			return fail()
		}
		name := "EVALOOM_SECRET_" + strings.ToUpper(declaration.Slot)
		if _, exists := env[name]; exists {
			return fail()
		}
		if stage == model.StagePrepare && declaration.Access != "read" {
			return fail()
		}
		access, value, err := s.load(harnessID, declaration.Slot)
		if err != nil {
			return fail()
		}
		if stage == model.StagePrepare && access != "read" {
			clear(value)
			return fail()
		}
		env[name] = string(value)
		clear(value)
	}
	return Resolved{env: env}, nil
}

// Keep accidental JSON serialization empty; values are accessible only through
// the explicit memory-only accessors above.
var _ fmt.Stringer = Resolved{}
