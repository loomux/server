package agents_test

import (
	"slices"
	"testing"

	"github.com/Loomux/server/agents"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// The effort levels are each CLI's own: Claude Code's --help lists low,
// medium, high, xhigh and max; Codex takes minimal to xhigh.
func TestAdapters_Efforts(t *testing.T) {
	for name, want := range map[string][]string{
		"claude-code": {"low", "medium", "high", "xhigh", "max"},
		"codex":       {"minimal", "low", "medium", "high", "xhigh"},
	} {
		at := agents.ClaudeCode()
		if name == "codex" {
			at = agents.Codex()
		}
		if at.LaunchArgs == nil || !slices.Equal(at.LaunchArgs.Efforts, want) {
			t.Errorf("%s efforts = %+v, want %q", name, at.LaunchArgs, want)
		}
		reg := router.AgentTypeRegistry{name: at}
		for _, e := range want {
			if err := reg.CheckAgentArgs(name, registry.AgentArgs{Effort: e}); err != nil {
				t.Errorf("%s effort %q: %v", name, e, err)
			}
		}
	}
	reg := router.AgentTypeRegistry{"claude-code": agents.ClaudeCode(), "codex": agents.Codex()}
	if err := reg.CheckAgentArgs("claude-code", registry.AgentArgs{Effort: "minimal"}); err == nil {
		t.Error("claude-code took effort minimal")
	}
	if err := reg.CheckAgentArgs("codex", registry.AgentArgs{Effort: "max"}); err == nil {
		t.Error("codex took effort max")
	}
}
