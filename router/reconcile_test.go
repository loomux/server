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

// reconcileHarness: a router over a fake executor with a claude-code
// (marker-tier) agent, and a helper to plant a task a previous loomuxd
// left running.
func reconcileHarness(t *testing.T) (registry.Store, *fakeExecutor, *router.Router, *registry.Workspace) {
	t.Helper()
	store := newTestStore(t)
	exec := newFakeExecutor()
	agentTypes := router.AgentTypeRegistry{
		"":            router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond}},
		"claude-code": router.AgentType{AgentConfig: completion.AgentConfig{Tier: completion.TierMarker}, LaunchTemplate: "claude"},
	}
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, exec.factory(), agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, exec.factory(), detector)
	model := &routertest.StubRoutingModel{
		RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
			return router.RelayResult{Reply: "relayed: " + captured}, nil
		},
	}
	r := router.New(store, orch, exec.factory(), credentials.NewResolver(store), agentTypes, model, markerDir)
	return store, exec, r, createFixtureWorkspace(t, store)
}

func plantTask(t *testing.T, store registry.Store, exec *fakeExecutor, ws *registry.Workspace, kind registry.TaskKind, live bool) *registry.Task {
	t.Helper()
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: kind, TmuxSession: "loomux-" + uuid.NewString(),
		Status: registry.TaskStatusRunning, ConversationID: "conv-1"}
	if kind == registry.TaskKindAgent {
		task.AgentType = "claude-code"
	}
	if err := store.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if live {
		_ = exec.NewSession(context.Background(), task.TmuxSession, ws.Path, "claude")
	}
	return task
}

// LOOM-82: a dispatch left running whose agent is still working is
// carried on: once the agent finishes, its reply is relayed and logged.
func TestResumeDispatch_LiveAgent(t *testing.T) {
	store, exec, r, ws := reconcileHarness(t)
	task := plantTask(t, store, exec, ws, registry.TaskKindAgent, true)
	exec.set(func() { exec.capture, exec.fileExists = "the agent's answer", true })
	if err := store.CreateDispatch(context.Background(), &registry.Dispatch{ID: "d1", ConversationID: "conv-1", Message: "do it",
		RequestHash: "h", Status: registry.DispatchStatusQueued}, nil); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}

	taskID, run := r.ResumeDispatch(context.Background(), &registry.Dispatch{ID: "d1", ConversationID: "conv-1"})
	if run == nil || taskID != task.ID {
		t.Fatalf("ResumeDispatch = %q, %v; want the running task", taskID, run != nil)
	}
	reply, err := run(context.Background(), &registry.Dispatch{ID: "d1", ConversationID: "conv-1", Message: "do it"})
	if err != nil || reply != "relayed: the agent's answer" {
		t.Fatalf("resumed run = %q, %v", reply, err)
	}
	if got, _ := store.GetTask(context.Background(), task.ID); got.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("task status = %s, want awaiting-input", got.Status)
	}
	msgs, _ := store.ListMessagesByConversation(context.Background(), "conv-1")
	if len(msgs) != 1 || msgs[0].Role != registry.MessageRoleAssistant {
		t.Errorf("messages = %+v, want only the reply (the user message was stored at submit)", msgs)
	}
}

// The agent's session vanished while loomuxd was down: the task is
// failed and the job fails saying so.
func TestResumeDispatch_SessionGone(t *testing.T) {
	store, exec, r, ws := reconcileHarness(t)
	task := plantTask(t, store, exec, ws, registry.TaskKindAgent, false)
	_, run := r.ResumeDispatch(context.Background(), &registry.Dispatch{ID: "d1", ConversationID: "conv-1"})
	if run == nil {
		t.Fatal("ResumeDispatch = nil, want a run that reports the lost session")
	}
	_, err := run(context.Background(), &registry.Dispatch{ID: "d1", ConversationID: "conv-1"})
	if err == nil || !strings.Contains(err.Error(), "restarted") {
		t.Errorf("err = %v, want it to say the session was lost in the restart", err)
	}
	assertFailed(t, mustTask(t, store, task.ID), registry.ErrorClassSessionLost, "restarted")
}

// No running agent task: nothing to resume.
func TestResumeDispatch_NothingRunning(t *testing.T) {
	_, _, r, _ := reconcileHarness(t)
	if _, run := r.ResumeDispatch(context.Background(), &registry.Dispatch{ID: "d1", ConversationID: "conv-1"}); run != nil {
		t.Error("ResumeDispatch returned a run for a conversation with nothing running")
	}
}

// ReconcileTasks fails running tasks whose sessions are gone, finishes a
// command that exited meanwhile, and leaves skipped and live tasks.
func TestReconcileTasks(t *testing.T) {
	store, exec, r, ws := reconcileHarness(t)
	gone := plantTask(t, store, exec, ws, registry.TaskKindAgent, false)
	skipped := plantTask(t, store, exec, ws, registry.TaskKindAgent, false)
	live := plantTask(t, store, exec, ws, registry.TaskKindAgent, true)
	cmd := plantTask(t, store, exec, ws, registry.TaskKindCommand, true)
	exec.set(func() {
		exec.sessions[cmd.TmuxSession].command = "df -h"
		exec.paneExit = func(command string) *targets.PaneExit {
			if command == "df -h" {
				return &targets.PaneExit{Status: 3}
			}
			return nil
		}
	})

	r.ReconcileTasks(context.Background(), map[string]bool{skipped.ID: true})

	assertFailed(t, mustTask(t, store, gone.ID), registry.ErrorClassSessionLost, "restarted")
	if got := mustTask(t, store, skipped.ID); got.Status != registry.TaskStatusRunning {
		t.Errorf("skipped task = %s, want left running", got.Status)
	}
	if got := mustTask(t, store, live.ID); got.Status != registry.TaskStatusRunning {
		t.Errorf("live agent task = %s, want left running", got.Status)
	}
	if got := mustTask(t, store, cmd.ID); got.Status != registry.TaskStatusCompleted || got.ExitCode == nil || *got.ExitCode != 3 {
		t.Errorf("command task = %s (exit %v), want completed with exit 3", got.Status, got.ExitCode)
	}
}

func mustTask(t *testing.T, store registry.Store, id string) *registry.Task {
	t.Helper()
	got, err := store.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return got
}

// Right after a restart the network sidecar may not be up yet: a target
// that can't be reached at first is retried, not given up on.
func TestReconcileTasks_RetriesUnreachableTarget(t *testing.T) {
	defer router.SetBootRetry(50*time.Millisecond, 5*time.Second)()
	store, exec, r, ws := reconcileHarness(t)
	gone := plantTask(t, store, exec, ws, registry.TaskKindAgent, false)
	exec.set(func() { exec.unreachableFor = 3 })

	r.ReconcileTasks(context.Background(), nil)

	assertFailed(t, mustTask(t, store, gone.ID), registry.ErrorClassSessionLost, "restarted")
}
