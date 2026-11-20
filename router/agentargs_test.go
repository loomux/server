package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// pinnableAgent takes a model and an effort the way a real adapter does:
// each a fixed flag followed by the one value.
func pinnableAgent() router.AgentType {
	return router.AgentType{
		LaunchTemplate: "agent",
		Profile:        router.LaunchProfile{PermissionArgs: []string{"--mode", "auto"}, PromptAsArg: true},
		LaunchArgs: &router.LaunchArgs{
			Model:   func(m string) []string { return []string{"--model", m} },
			Effort:  func(e string) []string { return []string{"--effort", e} },
			Efforts: []string{"low", "high"},
		},
	}
}

func pinTarget(t *testing.T, store registry.Store, targetID string, args map[string]registry.AgentArgs) {
	t.Helper()
	target, err := store.GetTarget(context.Background(), targetID)
	if err != nil {
		t.Fatal(err)
	}
	target.AgentArgs = args
	if err := store.UpdateTarget(context.Background(), target); err != nil {
		t.Fatalf("UpdateTarget: %v", err)
	}
}

// LOOM-307: the target's pin for the agent type goes on the launch, as
// the adapter's own flags, before the prompt; the task records it.
func TestDispatch_TargetAgentArgs(t *testing.T) {
	store, ws, exec, r := profileRouter(t, pinnableAgent())
	pin := registry.AgentArgs{Model: "opus", Effort: "high"}
	pinTarget(t, store, ws.TargetID, map[string]registry.AgentArgs{"claude-code": pin, "codex": {Model: "o3"}})

	if _, err := r.Dispatch(context.Background(), "conv-1", "-- --model fable"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cmd := onlySession(t, exec).command
	if !strings.Contains(cmd, `agent '--mode' 'auto' '--model' 'opus' '--effort' 'high'`) {
		t.Errorf("command = %q, want the pinned model and effort after the permission args", cmd)
	}
	if strings.Contains(cmd, "'o3'") {
		t.Errorf("command = %q carries another agent type's pin", cmd)
	}
	// The message stays one argument after "--": it can't add a flag.
	if !strings.HasSuffix(cmd, `'--' '-- --model fable'`) {
		t.Errorf("command = %q, want the message last, after --", cmd)
	}
	if got := onlyTask(t, store, ws.ID).AgentArgs; got != pin {
		t.Errorf("task.AgentArgs = %+v, want %+v", got, pin)
	}
}

// A model alone or an effort alone pins just that.
func TestDispatch_TargetAgentArgsPartial(t *testing.T) {
	store, ws, exec, r := profileRouter(t, pinnableAgent())
	pinTarget(t, store, ws.TargetID, map[string]registry.AgentArgs{"claude-code": {Effort: "low"}})
	if _, err := r.Dispatch(context.Background(), "conv-1", "do the work"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cmd := onlySession(t, exec).command
	if !strings.Contains(cmd, `'--effort' 'low'`) || strings.Contains(cmd, "--model") {
		t.Errorf("command = %q, want only the pinned effort", cmd)
	}
}

// No pin, no flags, and the task records none.
func TestDispatch_NoAgentArgs(t *testing.T) {
	store, ws, exec, r := profileRouter(t, pinnableAgent())
	if _, err := r.Dispatch(context.Background(), "conv-1", "do the work"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmd := onlySession(t, exec).command; strings.Contains(cmd, "--model") || strings.Contains(cmd, "--effort") {
		t.Errorf("command = %q, want no model or effort flags", cmd)
	}
	if got := onlyTask(t, store, ws.ID).AgentArgs; !got.IsZero() {
		t.Errorf("task.AgentArgs = %+v, want zero", got)
	}
}

// A pin the agent type can't honour fails the launch: running on the
// account's defaults is what the pin is there to stop.
func TestDispatch_RefusesAPinTheAgentCantTake(t *testing.T) {
	noArgs := pinnableAgent()
	noArgs.LaunchArgs = nil
	noModel := pinnableAgent()
	noModel.LaunchArgs.Model = nil
	for _, tc := range []struct {
		name    string
		at      router.AgentType
		pin     registry.AgentArgs
		wantErr string
	}{
		{"effort not offered", pinnableAgent(), registry.AgentArgs{Effort: "max"},
			`effort "max" is not one claude-code takes (low, high)`},
		{"agent takes no pins", noArgs, registry.AgentArgs{Model: "opus"}, "claude-code takes no model or effort"},
		{"agent takes no model", noModel, registry.AgentArgs{Model: "opus"}, "claude-code takes no model"},
		{"model not a model", pinnableAgent(), registry.AgentArgs{Model: "opus --x"}, "model " + registry.AgentModelRule},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, ws, exec, r := profileRouter(t, tc.at)
			// Written past the API's checks, as an older binary or a
			// changed adapter would leave it.
			target, _ := store.GetTarget(context.Background(), ws.TargetID)
			target.AgentArgs = map[string]registry.AgentArgs{"claude-code": tc.pin}
			if err := store.UpdateTarget(context.Background(), target); err != nil {
				t.Fatalf("UpdateTarget: %v", err)
			}
			_, err := r.Dispatch(context.Background(), "conv-1", "do the work")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Dispatch = %v, want an error containing %q", err, tc.wantErr)
			}
			if len(exec.sessions) != 0 {
				t.Errorf("a session started: %d", len(exec.sessions))
			}
		})
	}
}

func TestAgentTypeRegistryCheckAgentArgs(t *testing.T) {
	reg := router.AgentTypeRegistry{"claude-code": pinnableAgent()}
	if err := reg.CheckAgentArgs("claude-code", registry.AgentArgs{Model: "sonnet[1m]", Effort: "high"}); err != nil {
		t.Errorf("CheckAgentArgs(valid) = %v", err)
	}
	if err := reg.CheckAgentArgs("claude-code", registry.AgentArgs{}); err != nil {
		t.Errorf("CheckAgentArgs(zero) = %v, want nil", err)
	}
	if err := reg.CheckAgentArgs("nope", registry.AgentArgs{Model: "opus"}); err == nil {
		t.Error("CheckAgentArgs(unknown agent type) = nil")
	}
	if err := reg.CheckAgentArgs("claude-code", registry.AgentArgs{Effort: "medium"}); err == nil {
		t.Error("CheckAgentArgs(effort not offered) = nil")
	}
}
