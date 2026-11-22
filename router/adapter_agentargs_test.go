package router_test

import (
	"strings"
	"testing"

	"github.com/Loomux/server/agents"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// LOOM-307: each adapter maps a target's pinned model and effort to its
// CLI's own flags, each value one argument, before the first prompt.
func TestAdapters_PinnedArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   router.AgentType
		pin  registry.AgentArgs
		want string
	}{
		// Claude Code 2.1.329 --help: --model <model>, --effort <level>.
		{"claude-code", agents.ClaudeCode(), registry.AgentArgs{Model: "opus", Effort: "high"},
			`'--model' 'opus' '--effort' 'high'`},
		{"claude-code", agents.ClaudeCode(), registry.AgentArgs{Model: "sonnet[1m]"}, `'--model' 'sonnet[1m]'`},
		// Codex 0.163.0: -m <MODEL>; -c parses its value as TOML, so the
		// effort is a TOML string.
		{"codex", agents.Codex(), registry.AgentArgs{Model: "gpt-5.1-codex", Effort: "medium"},
			`'-m' 'gpt-5.1-codex' '-c' 'model_reasoning_effort="medium"'`},
		{"codex", agents.Codex(), registry.AgentArgs{Effort: "xhigh"}, `'-c' 'model_reasoning_effort="xhigh"'`},
	} {
		got, err := tc.at.LaunchCommandWithArgsForTest(tc.name, "/srv/ws", "do the work", registry.PermissionModeAuto, tc.pin)
		if err != nil {
			t.Errorf("%s %+v: %v", tc.name, tc.pin, err)
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s %+v: command %q does not contain %s", tc.name, tc.pin, got, tc.want)
		}
		if !strings.HasSuffix(got, `'--' 'do the work'`) {
			t.Errorf("%s: command %q does not end with the first prompt", tc.name, got)
		}
	}
}
