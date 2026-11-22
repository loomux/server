package app

import (
	"testing"

	"github.com/Loomux/server/registry"
)

// LOOM-307: the API checks a target's pins against the agent types this
// server launches, each with its own CLI's levels; never the registry's
// internal "" fallback entry.
func TestCheckAgentArgs(t *testing.T) {
	agentTypes := DefaultAgentTypes()
	a := &App{agentRegistry: agentTypes, agentTypes: dispatchableAgentTypeNames(agentTypes)}
	for _, tc := range []struct {
		agentType string
		pin       registry.AgentArgs
		ok        bool
	}{
		{"claude-code", registry.AgentArgs{Model: "opus", Effort: "max"}, true},
		{"codex", registry.AgentArgs{Model: "gpt-5.1-codex", Effort: "minimal"}, true},
		{"claude-code", registry.AgentArgs{Effort: "minimal"}, false},
		{"codex", registry.AgentArgs{Effort: "max"}, false},
		{"gemini", registry.AgentArgs{Model: "pro"}, false},
		{"", registry.AgentArgs{Model: "opus"}, false},
	} {
		if err := a.CheckAgentArgs(tc.agentType, tc.pin); (err == nil) != tc.ok {
			t.Errorf("CheckAgentArgs(%q, %+v) = %v, want ok=%v", tc.agentType, tc.pin, err, tc.ok)
		}
	}
}
