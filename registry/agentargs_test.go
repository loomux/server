package registry_test

import (
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
)

// LOOM-307: a target pins an agent type's model and effort. The model is
// one argv element after a fixed flag, so it may hold only what a model
// name needs: no flag, no shell metacharacter, no whitespace.
func TestValidateAgentModel(t *testing.T) {
	for model, ok := range map[string]bool{
		"opus": true, "sonnet": true, "claude-opus-5-5": true, "sonnet[1m]": true, "gpt-5.1-codex": true,
		"o3": true, "us.anthropic.claude-opus-5-5-v1:0": true, strings.Repeat("a", 64): true,

		"": false, strings.Repeat("a", 65): false, "-m": false, "--model": false, ".opus": false,
		"opus max": false, "opus;rm": false, "opus|x": false, "opus&": false, "$(id)": false, "`id`": false,
		"opus\n": false, "opus'": false, `opus"`: false, "a=b": false, "op*": false, "op?": false,
		"opus[2m]": false, "[1m]": false, "op/us": false, "opus\\": false, "opus\t": false, "ópus": false,
	} {
		if err := registry.ValidateAgentModel(model); (err == nil) != ok {
			t.Errorf("ValidateAgentModel(%q) = %v, want ok=%v", model, err, ok)
		}
	}
}

func TestTargetValidate_AgentArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    map[string]registry.AgentArgs
		wantErr string
	}{
		{name: "none", args: nil},
		{name: "empty map", args: map[string]registry.AgentArgs{}},
		{name: "model and effort", args: map[string]registry.AgentArgs{"claude-code": {Model: "opus", Effort: "high"}}},
		{name: "model only", args: map[string]registry.AgentArgs{"codex": {Model: "gpt-5.1-codex"}}},
		{name: "effort only", args: map[string]registry.AgentArgs{"codex": {Effort: "medium"}}},
		{name: "blank agent type", args: map[string]registry.AgentArgs{" ": {Model: "opus"}},
			wantErr: "agent_args must not contain blank or padded agent type names"},
		{name: "padded agent type", args: map[string]registry.AgentArgs{"codex ": {Model: "o3"}},
			wantErr: "agent_args must not contain blank or padded agent type names"},
		{name: "empty entry", args: map[string]registry.AgentArgs{"codex": {}},
			wantErr: `agent_args["codex"]: give a model, an effort or both (leave the agent type out to pin nothing)`},
		{name: "bad model", args: map[string]registry.AgentArgs{"claude-code": {Model: "opus --dangerously-skip-permissions"}},
			wantErr: `agent_args["claude-code"].model: ` + registry.AgentModelRule},
		{name: "bad effort", args: map[string]registry.AgentArgs{"claude-code": {Effort: "High"}},
			wantErr: `agent_args["claude-code"].effort: ` + registry.AgentEffortRule},
		{name: "effort with a flag", args: map[string]registry.AgentArgs{"claude-code": {Effort: "high --x"}},
			wantErr: `agent_args["claude-code"].effort: ` + registry.AgentEffortRule},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := &registry.Target{Name: "t", Kind: registry.TargetKindLocal, AgentArgs: tc.args}
			err := target.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Validate() = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
