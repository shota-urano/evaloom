package registry

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

// ToolPermissionsChecker is implemented by an Adapter that converts
// stages.work.toolPermissions to its CLI settings (spec 08 3.9, Q145).
type ToolPermissionsChecker interface {
	CheckToolPermissions(json.RawMessage) error
}

// ModelAPIDeclarer is implemented by an Adapter that declares the model API
// destinations of its CLI as host:port (spec 08 3.6・3.7, spec 09 3.3, Q146).
type ModelAPIDeclarer interface {
	ModelAPIDestinations() []string
}

// DefaultStageTimeoutMaxSec is the default of the global setting
// limits.stageTimeoutMaxSec (spec 02 3.8).
var DefaultStageTimeoutMaxSec = map[model.Stage]int{
	model.StagePrepare: 1800, model.StageWork: 14400, model.StageCheck: 3600, model.StagePublish: 1800,
}

// ParseStageTimeoutMaxSec reads the settings value of
// limits.stageTimeoutMaxSec. A missing row uses the default. A stage the value
// does not give a positive integer for has no allowed timeout (Fail Closed).
func ParseStageTimeoutMaxSec(raw string, present bool) map[model.Stage]int {
	if !present {
		return DefaultStageTimeoutMaxSec
	}
	var values map[string]json.RawMessage
	limits := map[model.Stage]int{}
	if json.Unmarshal([]byte(raw), &values) != nil {
		return limits
	}
	for stage := range DefaultStageTimeoutMaxSec {
		var n int
		if json.Unmarshal(values[string(stage)], &n) == nil && n > 0 {
			limits[stage] = n
		}
	}
	return limits
}

// Environment is the deployment side of Profile registration (spec 03 3.4).
type Environment struct {
	StageTimeoutMaxSecs map[model.Stage]int
	// ToolPermissions holds the conversion check of each registered Adapter.
	// Without one, only empty Tool Permissions can be accepted.
	ToolPermissions map[model.AdapterName]ToolPermissionsChecker
	// ModelAPIs is the union of model API destinations of all registered
	// Adapters. prepare and publish cannot declare them.
	ModelAPIs map[string]bool
}

var _ contract.ProfileEnvironment = Environment{}

func (e Environment) StageTimeoutMaxSec(stage model.Stage) int { return e.StageTimeoutMaxSecs[stage] }

func (e Environment) ValidateToolPermissions(name model.AdapterName, raw json.RawMessage) error {
	if checker := e.ToolPermissions[name]; checker != nil {
		return checker.CheckToolPermissions(raw)
	}
	var permissions map[string][]string
	if err := json.Unmarshal(raw, &permissions); err != nil {
		return err
	}
	for _, values := range permissions {
		if len(values) > 0 {
			return fmt.Errorf("adapter %q cannot convert tool permissions", name)
		}
	}
	return nil
}

// ValidateRuntime rejects macos_vm until the macOS VM is implemented
// (spec 09 3.4, Q147).
func (Environment) ValidateRuntime(runtime contract.Runtime) error {
	if runtime.Kind != "container" {
		return errors.New("runtime kind is not supported")
	}
	return nil
}

func (e Environment) ValidateNetworkDestination(destination string) error {
	if e.ModelAPIs[destination] {
		return errors.New("model API destination")
	}
	return nil
}
