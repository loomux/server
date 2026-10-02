package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
	"github.com/Loomux/server/targets"
)

// TestIntegration_RunCommand is LOOM-72 end to end against real tmux:
// "run `…` on <target>" runs the command in a tmux pane with no agent,
// completes on its exit, relays its output and exit code verbatim, and
// tears the pane down.
func TestIntegration_RunCommand(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	target := &registry.Target{ID: uuid.NewString(), Name: "local", Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	agentTypes := router.AgentTypeRegistry{"": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle}}}
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, targets.NewExecutor, agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, targets.NewExecutor, detector)
	const command = `echo "loomux-$((6*7))"; echo to-stderr >&2; exit 4`
	model := &routertest.StubRoutingModel{
		DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return router.Decision{Action: router.ActionRunCommand, TargetID: target.ID, Command: command}, nil
		},
	}
	r := router.New(store, orch, targets.NewExecutor, credentials.NewResolver(store), agentTypes, model, markerDir)
	exec := targets.NewLocalExecutor()
	t.Cleanup(func() { killWorkspaceSessions(t, store, exec) })

	reply, err := r.Dispatch(ctx, "conv", "run `"+command+"` on local")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	for _, want := range []string{"loomux-42", "to-stderr", "exit 4"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
	tasks, err := store.ListTasks(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %v, %v", tasks, err)
	}
	task := tasks[0]
	if task.Kind != registry.TaskKindCommand || task.Command != command || task.ExitCode == nil || *task.ExitCode != 4 {
		t.Errorf("task = %+v (exit %v), want the command and exit code 4 recorded", task, task.ExitCode)
	}
	if alive, _ := exec.HasSession(ctx, task.TmuxSession); alive {
		t.Error("the command's session was left running after it finished")
	}
}
