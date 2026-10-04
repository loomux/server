package router_test

import (
	"context"
	"testing"
	"time"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
)

// timeoutHarness: a claude-code turn that never signals completion and
// times out after 300ms.
func timeoutHarness(t *testing.T) (registry.Store, *fakeExecutor, *router.Router, *routertest.StubRoutingModel, *registry.Workspace) {
	t.Helper()
	store := newTestStore(t)
	exec := newFakeExecutor()
	agentTypes := router.AgentTypeRegistry{
		"": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond}},
		"claude-code": router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierMarker, MaxTurnDuration: 300 * time.Millisecond, NoProgressTimeout: -1},
			LaunchTemplate: "claude",
			InterruptKeys:  []string{"Escape"},
		},
	}
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, exec.factory(), agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, exec.factory(), detector)
	model := &routertest.StubRoutingModel{}
	r := router.New(store, orch, exec.factory(), credentials.NewResolver(store), agentTypes, model, markerDir)
	ws := createFixtureWorkspace(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	return store, exec, r, model, ws
}

// LOOM-117: a timed-out turn's agent is interrupted — it must not carry
// on unsupervised — while its pane is kept for inspection.
func TestDispatch_TurnTimeout_InterruptsAgent(t *testing.T) {
	store, exec, r, _, ws := timeoutHarness(t)
	if _, err := r.Dispatch(context.Background(), "conv-1", "do something long"); err == nil {
		t.Fatal("Dispatch: nil error for a turn that never completed")
	}
	task := onlyTask(t, store, ws.ID)
	if keys := exec.sessionFor(task.TmuxSession).namedKeys; len(keys) == 0 || keys[len(keys)-1] != "Escape" {
		t.Errorf("named keys sent to the timed-out pane = %v, want Escape", keys)
	}
	if alive, _ := exec.HasSession(context.Background(), task.TmuxSession); !alive {
		t.Error("the timed-out pane was torn down; it must be kept")
	}
}

// LOOM-117: the next turn into that workspace retires the timed-out
// pane before launching, so there's never a second agent in it.
func TestDispatch_AfterTimeout_StalePaneRetiredBeforeLaunch(t *testing.T) {
	store, exec, r, _, ws := timeoutHarness(t)
	if _, err := r.Dispatch(context.Background(), "conv-1", "first"); err == nil {
		t.Fatal("first Dispatch: want the timeout")
	}
	stale := onlyTask(t, store, ws.ID)

	// Another conversation, same workspace: still the same directory.
	_, _ = r.Dispatch(context.Background(), "conv-2", "second")

	if alive, _ := exec.HasSession(context.Background(), stale.TmuxSession); alive {
		t.Error("the timed-out pane is still alive after a new launch in its workspace")
	}
	got, _ := store.GetTask(context.Background(), stale.ID)
	if got.ReapedAt == nil || got.Status != registry.TaskStatusFailed {
		t.Errorf("stale task = %+v, want still failed with reaped_at set", got)
	}
	tasks, _ := store.ListTasksByWorkspace(context.Background(), ws.ID)
	live := 0
	for _, task := range tasks {
		if ok, _ := exec.HasSession(context.Background(), task.TmuxSession); ok {
			live++
		}
	}
	if live > 1 {
		t.Errorf("%d live panes in one workspace, want at most 1", live)
	}
}
