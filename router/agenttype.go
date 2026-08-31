// Package router wires the workspace registry, orchestrator, completion
// detection, and credential vault together into the dispatch pipeline
// (design spec §2, §6): given an incoming chat message, decide which
// workspace to use (or provision a new one), launch or provision via
// the orchestrator, and relay the result back. See router/README.md.
package router

import (
	"fmt"

	"github.com/Loomux/server/completion"
)

// AgentType is a full registered agent-type entry (design spec §6:
// "each registered agent-type supplies a launch command template and a
// completion-detection adapter"). It composes completion.AgentConfig
// (LOOM-6's completion-detection half) rather than duplicating its
// fields, so an agent-type's tier/timeout has exactly one source of
// truth.
type AgentType struct {
	completion.AgentConfig
	// LaunchTemplate is the already-runnable command that starts this
	// agent-type's CLI (e.g. "claude"). A literal string, not a
	// templating DSL with substitutable placeholders — nothing in this
	// package needs per-launch parameterization inside the command
	// itself (the workspace directory is handled separately by
	// NewSession's own dir parameter; credentials are prefixed
	// separately via credentials.ShellEnvPrefix, not templated in).
	LaunchTemplate string
	// VersionCheck, if set, is run before every launch of this
	// agent-type (design spec §10 axis 3). nil means no check is
	// enforced.
	VersionCheck *VersionCheck
}

// AgentTypeRegistry maps agent-type name to its AgentType entry. An
// entry keyed by the empty string configures completion detection for
// every shell-kind task (provisioning), since registry.Task.AgentType is
// always "" for those and completion.Detector looks tasks up by exactly
// that field — the only way to reach shell tasks through this
// mechanism.
type AgentTypeRegistry map[string]AgentType

// CompletionConfig projects this registry into the narrower
// completion.Config shape completion.NewDetector needs. Always derived
// from the registry, never hand-maintained separately.
func (r AgentTypeRegistry) CompletionConfig() completion.Config {
	cfg := make(completion.Config, len(r))
	for name, at := range r {
		cfg[name] = at.AgentConfig
	}
	return cfg
}

// Get returns the full AgentType entry for agentType, or an error if it
// isn't registered.
func (r AgentTypeRegistry) Get(agentType string) (AgentType, error) {
	at, ok := r[agentType]
	if !ok {
		return AgentType{}, fmt.Errorf("router: unknown agent type %q", agentType)
	}
	return at, nil
}

// LaunchCommand returns the resolved launch command for agentType, or
// an error if it isn't registered.
func (r AgentTypeRegistry) LaunchCommand(agentType string) (string, error) {
	at, err := r.Get(agentType)
	if err != nil {
		return "", err
	}
	return at.LaunchTemplate, nil
}
