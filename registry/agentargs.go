package registry

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// AgentArgs pin how an agent type's CLI is launched on a target
// (LOOM-307): the model it runs and its effort. Each field is optional;
// empty leaves the CLI's (the account's) own default. They are the only
// launch settings a target can pin, each passed as one argument after a
// flag its agent type fixes, never as free text. Only the target API
// sets them: the router models and prompts never do.
type AgentArgs struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// IsZero reports whether a pins nothing.
func (a AgentArgs) IsZero() bool { return a == AgentArgs{} }

// AgentModelRule is the message for a model ValidateAgentModel refuses.
const AgentModelRule = "must be 1-64 letters, digits, '.', '_', ':' or '-', starting with a letter or digit, " +
	"optionally ending in [1m]"

// AgentEffortRule is the message for an effort the registry refuses. Each
// agent type further allows only its own CLI's levels.
const AgentEffortRule = "must be 1-16 lower-case letters"

var (
	agentModelRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*(\[1m\])?$`)
	agentEffortRE = regexp.MustCompile(`^[a-z]{1,16}$`)
)

// ValidateAgentModel checks a pinned model name: a single word that can't
// be read as a flag or carry a shell metacharacter. "[1m]" is Claude
// Code's 1M-context suffix (sonnet[1m]).
func ValidateAgentModel(model string) error {
	if len(model) > 64 || !agentModelRE.MatchString(model) {
		return errors.New(AgentModelRule)
	}
	return nil
}

// validateAgentArgs is Target.Validate's check of AgentArgs: agent type
// names unpadded, each entry pinning something, each value well-formed.
// Whether an agent type exists and takes that effort is for the caller
// that knows the agent types (the API, and the launch itself).
func validateAgentArgs(args map[string]AgentArgs) error {
	names := make([]string, 0, len(args))
	for name := range args {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
			return errors.New("agent_args must not contain blank or padded agent type names")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a := args[name]
		switch {
		case a.IsZero():
			return fmt.Errorf("agent_args[%q]: give a model, an effort or both (leave the agent type out to pin nothing)", name)
		case a.Model != "" && ValidateAgentModel(a.Model) != nil:
			return fmt.Errorf("agent_args[%q].model: %s", name, AgentModelRule)
		case a.Effort != "" && !agentEffortRE.MatchString(a.Effort):
			return fmt.Errorf("agent_args[%q].effort: %s", name, AgentEffortRule)
		}
	}
	return nil
}
