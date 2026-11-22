package router

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Loomux/server/registry"
)

// LaunchArgs is how an agent type's CLI takes the model and effort a
// target pins for it (LOOM-307, registry.Target.AgentArgs). Each func
// returns the CLI's own flags for one value, which is passed as a single
// argument; nothing else from the pin reaches the command line.
type LaunchArgs struct {
	// Model returns the flags that select model (registry.ValidateAgentModel
	// has accepted it). nil: the CLI can't be given a model.
	Model func(model string) []string
	// Effort returns the flags that set effort, one of Efforts. nil: the
	// CLI can't be given an effort.
	Effort func(effort string) []string
	// Efforts are the effort levels the CLI takes, as it spells them.
	Efforts []string
}

// CheckAgentArgs reports whether agentType is registered and its CLI
// takes the pin a; the zero pin always passes for a registered type.
func (r AgentTypeRegistry) CheckAgentArgs(agentType string, a registry.AgentArgs) error {
	at, err := r.Get(agentType)
	if err != nil {
		return fmt.Errorf("unknown agent type %q", agentType)
	}
	_, err = at.pinnedArgs(agentType, a)
	return err
}

// pinnedArgs are the CLI flags for the pin a, or why the CLI can't take
// it. name is the agent type's name, for the message.
func (at AgentType) pinnedArgs(name string, a registry.AgentArgs) ([]string, error) {
	if a.IsZero() {
		return nil, nil
	}
	la := at.LaunchArgs
	if la == nil {
		return nil, fmt.Errorf("%s takes no model or effort", name)
	}
	var args []string
	if a.Model != "" {
		if la.Model == nil {
			return nil, fmt.Errorf("%s takes no model", name)
		}
		if err := registry.ValidateAgentModel(a.Model); err != nil {
			return nil, fmt.Errorf("model %w", err)
		}
		args = append(args, la.Model(a.Model)...)
	}
	if a.Effort != "" {
		if la.Effort == nil {
			return nil, fmt.Errorf("%s takes no effort", name)
		}
		if !slices.Contains(la.Efforts, a.Effort) {
			return nil, fmt.Errorf("effort %q is not one %s takes (%s)", a.Effort, name, strings.Join(la.Efforts, ", "))
		}
		args = append(args, la.Effort(a.Effort)...)
	}
	return args, nil
}
