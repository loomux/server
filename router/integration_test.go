package router_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
	"github.com/Loomux/server/targets"
)

// TestIntegration_RealDispatch proves the full pipeline works
// end-to-end against real infrastructure: a real sqlite.Store, a real
// local tmux session via LOOM-4's LocalExecutor, a real tiered
// completion.Detector (not manually signaled), and a real
// credentials.Resolver — only the RoutingModel is a deterministic stub,
// per this ticket's explicit scope.
func TestIntegration_RealDispatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	target := &registry.Target{ID: uuid.NewString(), Name: "real-target-" + t.Name(), Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	ws := &registry.Workspace{
		ID:       uuid.NewString(),
		Name:     "real-ws-" + t.Name(),
		Path:     t.TempDir(), // a real directory: tmux's -c requires one to exist
		TargetID: target.ID,
		Status:   registry.WorkspaceStatusIdle,
	}
	if err := store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	agentTypes := router.AgentTypeRegistry{
		"echo-agent": router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 300 * time.Millisecond},
			LaunchTemplate: `sh -c 'echo integration-test-output; sleep 30'`,
		},
	}
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, targets.NewExecutor, agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, targets.NewExecutor, detector)
	resolver := credentials.NewResolver(store)
	model := &routertest.StubRoutingModel{
		DecideFunc: func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
			return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "echo-agent"}, nil
		},
		RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
			return router.RelayResult{Reply: "relayed: " + strings.TrimSpace(captured), Done: true}, nil
		},
	}

	r := router.New(store, orch, targets.NewExecutor, resolver, agentTypes, model, markerDir)

	reply, err := r.Dispatch(ctx, "conv-real", "do the real thing")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(reply, "integration-test-output") {
		t.Fatalf("reply = %q, want it to contain the real pane's output", reply)
	}

	// Completion was genuinely detected (not manually signaled) — proven
	// by having reached this point at all within the test's own
	// deadline, plus the pane's real output showing up in the reply.

	realExec := targets.NewLocalExecutor()
	t.Cleanup(func() {
		if err := realExec.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// Find the task Dispatch created, to check the real session is gone.
	tasks, err := store.ListTasksByWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("ListTasksByWorkspace = %+v, want exactly 1 task", tasks)
	}
	task := tasks[0]
	if task.Status != registry.TaskStatusCompleted {
		t.Fatalf("task Status = %q, want %q", task.Status, registry.TaskStatusCompleted)
	}

	exists, err := realExec.HasSession(ctx, task.TmuxSession)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	// Kept for the grace period after completing (LOOM-91).
	if !exists {
		t.Fatalf("real tmux session %q torn down at completion, want it kept for the grace period", task.TmuxSession)
	}
	t.Cleanup(func() { _ = realExec.KillSession(context.Background(), task.TmuxSession) })

	updatedWS, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if updatedWS.RollingSummary != reply {
		t.Fatalf("RollingSummary = %q, want it to match the reply %q", updatedWS.RollingSummary, reply)
	}
	if updatedWS.Status != registry.WorkspaceStatusIdle {
		t.Fatalf("workspace Status = %q, want %q", updatedWS.Status, registry.WorkspaceStatusIdle)
	}
}
