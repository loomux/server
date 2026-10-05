package router_test

import (
	"context"
	"errors"
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
// times out after 2s (seconds, so a slow CI runner can't make a cancelled
// turn look like a timed-out one).
func timeoutHarness(t *testing.T) (registry.Store, *fakeExecutor, *router.Router, *routertest.StubRoutingModel, *registry.Workspace) {
	t.Helper()
	store := newTestStore(t)
	exec := newFakeExecutor()
	agentTypes := router.AgentTypeRegistry{
		"": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond}},
		"claude-code": router.AgentType{
			AgentConfig:    completion.AgentConfig{Tier: completion.TierMarker, MaxTurnDuration: 2 * time.Second, NoProgressTimeout: -1},
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

// LOOM-99: a turn the user cancels ends at once: its task is failed with
// class cancelled and its agent interrupted, the pane kept to inspect.
func TestDispatch_UserCancel_InterruptsAndFailsCancelled(t *testing.T) {
	store, exec, r, _, ws := timeoutHarness(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	exec.onSendKeys = func() { time.AfterFunc(30*time.Millisecond, func() { cancel(orchestrator.ErrCancelled) }) }

	start := time.Now()
	if _, err := r.Dispatch(ctx, "conv-1", "do something long"); err == nil {
		t.Fatal("Dispatch: nil error for a cancelled turn")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("cancel took %s to end the turn, want it well before the 2s timeout", elapsed)
	}
	task := onlyTask(t, store, ws.ID)
	if task.Status != registry.TaskStatusFailed || task.ErrorClass != registry.ErrorClassCancelled {
		t.Errorf("task = %s/%s, want failed/cancelled", task.Status, task.ErrorClass)
	}
	if keys := exec.sessionFor(task.TmuxSession).namedKeys; len(keys) == 0 || keys[len(keys)-1] != "Escape" {
		t.Errorf("named keys sent to the cancelled pane = %v, want Escape", keys)
	}
	if alive, _ := exec.HasSession(context.Background(), task.TmuxSession); !alive {
		t.Error("the cancelled pane was torn down; it must be kept")
	}
}

// LOOM-99: a running task no dispatch is driving (a restart left it) is
// cancelled directly: failed with class cancelled, its agent interrupted.
func TestCancelTask_WithoutDispatch(t *testing.T) {
	store, exec, r, _, ws := timeoutHarness(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	exec.onSendKeys = func() { time.AfterFunc(30*time.Millisecond, func() { cancel(orchestrator.ErrInterrupted) }) }
	_, _ = r.Dispatch(ctx, "conv-1", "do something long")
	task := onlyTask(t, store, ws.ID)
	if task.Status != registry.TaskStatusRunning {
		t.Fatalf("task = %s, want left running by the shutdown", task.Status)
	}

	if err := r.CancelTask(context.Background(), task.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	task = onlyTask(t, store, ws.ID)
	if task.Status != registry.TaskStatusFailed || task.ErrorClass != registry.ErrorClassCancelled {
		t.Errorf("task = %s/%s, want failed/cancelled", task.Status, task.ErrorClass)
	}
	if keys := exec.sessionFor(task.TmuxSession).namedKeys; len(keys) == 0 || keys[len(keys)-1] != "Escape" {
		t.Errorf("named keys = %v, want Escape", keys)
	}
	if err := r.CancelTask(context.Background(), task.ID); !errors.Is(err, orchestrator.ErrTaskInactive) {
		t.Errorf("CancelTask(ended) = %v, want ErrTaskInactive", err)
	}
}

// LOOM-99 review: a task a person has taken over isn't cancelled; nothing
// is typed into the pane they are driving.
func TestCancelTask_RefusesHumanTakeover(t *testing.T) {
	store, exec, r, _, ws := timeoutHarness(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	exec.onSendKeys = func() { time.AfterFunc(30*time.Millisecond, func() { cancel(orchestrator.ErrInterrupted) }) }
	_, _ = r.Dispatch(ctx, "conv-1", "do something long")
	task := onlyTask(t, store, ws.ID)
	task.Status = registry.TaskStatusHumanTakeover
	if err := store.UpdateTask(context.Background(), task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	before := len(exec.sessionFor(task.TmuxSession).namedKeys)

	if err := r.CancelTask(context.Background(), task.ID); !errors.Is(err, orchestrator.ErrHumanTakeover) {
		t.Fatalf("CancelTask(taken over) = %v, want ErrHumanTakeover", err)
	}
	task = onlyTask(t, store, ws.ID)
	if task.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("task = %s, want still human-takeover", task.Status)
	}
	if n := len(exec.sessionFor(task.TmuxSession).namedKeys); n != before {
		t.Errorf("keys were sent to a taken-over pane")
	}
}
