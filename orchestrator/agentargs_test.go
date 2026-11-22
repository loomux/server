package orchestrator_test

import (
	"context"
	"testing"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// LOOM-307: an agent task records the model and effort it was launched
// with, from its creation on; a command task never carries any.
func TestLaunchWithID_RecordsAgentArgs(t *testing.T) {
	store, ws, _, _, o := setup(t)
	ctx := context.Background()
	pin := registry.AgentArgs{Model: "opus", Effort: "high"}

	task, err := o.LaunchWithID(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "pinned", "claude",
		orchestrator.WithAgentArgs(pin))
	if err != nil {
		t.Fatalf("LaunchWithID: %v", err)
	}
	if task.AgentArgs != pin {
		t.Errorf("task.AgentArgs = %+v, want %+v", task.AgentArgs, pin)
	}
	if stored, err := store.GetTask(ctx, "pinned"); err != nil || stored.AgentArgs != pin {
		t.Errorf("stored task = %+v, %v; want AgentArgs %+v", stored, err, pin)
	}

	cmd, err := o.LaunchWithID(ctx, ws.ID, "conv-1", registry.TaskKindCommand, "", "cmd", "true",
		orchestrator.WithAgentArgs(pin))
	if err != nil {
		t.Fatalf("LaunchWithID(command): %v", err)
	}
	if !cmd.AgentArgs.IsZero() {
		t.Errorf("command task.AgentArgs = %+v, want zero", cmd.AgentArgs)
	}
}
