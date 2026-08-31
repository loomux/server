package router_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// TestDispatch_NotDone_KeepsTaskOpenForFollowUp is LOOM-13's core case
// (design spec §3 steps 2-3): when Relay says Done: false, Dispatch
// must NOT tear the task down — it stays AwaitingInput, its session
// stays alive, the workspace stays Active, and the rolling summary is
// still updated. A second Dispatch call for the same workspace +
// conversation must then find that same task and send the follow-up
// message into it via SendMessage, not launch a second session.
func TestDispatch_NotDone_KeepsTaskOpenForFollowUp(t *testing.T) {
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "still working on it", Done: false}, nil
	}

	reply, err := r.Dispatch(context.Background(), "conv-1", "start the thing")
	if err != nil {
		t.Fatalf("Dispatch (turn 1): %v", err)
	}
	if reply != "still working on it" {
		t.Fatalf("reply = %q, want %q", reply, "still working on it")
	}

	if len(exec.sessions) != 1 {
		t.Fatalf("sessions = %+v, want exactly 1", exec.sessions)
	}
	var sess *fakeSession
	for _, s := range exec.sessions {
		sess = s
	}
	if !sess.alive {
		t.Fatal("session torn down after Done: false, want it left alive")
	}

	tasks, err := store.ListTasksByWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("ListTasksByWorkspace = %+v, want exactly 1 task", tasks)
	}
	firstTaskID := tasks[0].ID
	if tasks[0].Status != registry.TaskStatusAwaitingInput {
		t.Fatalf("task Status = %q, want %q", tasks[0].Status, registry.TaskStatusAwaitingInput)
	}

	updatedWS, err := store.GetWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if updatedWS.RollingSummary != "still working on it" {
		t.Fatalf("RollingSummary = %q, want %q", updatedWS.RollingSummary, "still working on it")
	}
	if updatedWS.Status != registry.WorkspaceStatusActive {
		t.Fatalf("workspace Status = %q, want %q (not reverted to idle since the task isn't done)", updatedWS.Status, registry.WorkspaceStatusActive)
	}

	// Turn 2: same conversation + workspace. Relay now says Done: true.
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "all done", Done: true}, nil
	}

	reply, err = r.Dispatch(context.Background(), "conv-1", "keep going")
	if err != nil {
		t.Fatalf("Dispatch (turn 2): %v", err)
	}
	if reply != "all done" {
		t.Fatalf("reply = %q, want %q", reply, "all done")
	}

	// No new session was created — the follow-up went into the same one.
	if len(exec.sessions) != 1 {
		t.Fatalf("sessions = %+v, want still exactly 1 (no new session launched)", exec.sessions)
	}
	if !contains(sess.keys, "keep going") {
		t.Fatalf("session keys = %+v, want them to include the follow-up message %q", sess.keys, "keep going")
	}
	if sess.alive {
		t.Fatal("session still alive after Done: true, want torn down")
	}

	tasks, err = store.ListTasksByWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != firstTaskID {
		t.Fatalf("ListTasksByWorkspace = %+v, want the same single task reused (id %q)", tasks, firstTaskID)
	}
	if tasks[0].Status != registry.TaskStatusCompleted {
		t.Fatalf("task Status = %q, want %q", tasks[0].Status, registry.TaskStatusCompleted)
	}

	updatedWS, err = store.GetWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if updatedWS.RollingSummary != "all done" {
		t.Fatalf("RollingSummary = %q, want %q", updatedWS.RollingSummary, "all done")
	}
	if updatedWS.Status != registry.WorkspaceStatusIdle {
		t.Fatalf("workspace Status = %q, want %q", updatedWS.Status, registry.WorkspaceStatusIdle)
	}
}

// TestDispatch_StaleSessionAfterReap_FallsBackToFreshLaunch is LOOM-16's
// core case: a task is left open (Done: false) same as any continuation,
// but by the time a follow-up message arrives its session is gone (idle
// reaped, crashed, manually killed — Dispatch can't tell which, and
// doesn't need to). Router must not error out or get stuck on the
// ambiguous-active-task guard; it fails the stale task and transparently
// launches a fresh one for the same conversation, so the conversation
// itself keeps working across a reap.
func TestDispatch_StaleSessionAfterReap_FallsBackToFreshLaunch(t *testing.T) {
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "still working on it", Done: false}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "start the thing"); err != nil {
		t.Fatalf("Dispatch (turn 1): %v", err)
	}

	tasks, err := store.ListTasksByWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("ListTasksByWorkspace = %+v, want exactly 1 task after turn 1", tasks)
	}
	staleTask := tasks[0]

	// Simulate the session vanishing out-of-band — exactly what
	// orchestrator.Reap does (tested directly at that layer); here we
	// only care that Router copes when it finds this state, regardless
	// of what caused it.
	if err := exec.KillSession(context.Background(), staleTask.TmuxSession); err != nil {
		t.Fatalf("KillSession (simulating a reap): %v", err)
	}

	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "picked back up", Done: true}, nil
	}

	reply, err := r.Dispatch(context.Background(), "conv-1", "keep going")
	if err != nil {
		t.Fatalf("Dispatch (turn 2, after simulated reap): %v", err)
	}
	if reply != "picked back up" {
		t.Fatalf("reply = %q, want %q", reply, "picked back up")
	}

	// A fresh session was launched — not a second attempt to use the
	// dead one.
	if len(exec.sessions) != 2 {
		t.Fatalf("sessions = %+v, want 2 (the dead one plus a fresh one)", exec.sessions)
	}

	tasks, err = store.ListTasksByWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace (after fallback): %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("ListTasksByWorkspace = %+v, want 2 tasks (the stale one plus a fresh one)", tasks)
	}
	var freshTask *registry.Task
	for _, tk := range tasks {
		if tk.ID == staleTask.ID {
			if tk.Status != registry.TaskStatusFailed {
				t.Fatalf("stale task Status = %q, want %q (so findActiveTask stops finding it)", tk.Status, registry.TaskStatusFailed)
			}
			continue
		}
		freshTask = tk
	}
	if freshTask == nil {
		t.Fatal("no second (fresh) task found")
	}
	if freshTask.Status != registry.TaskStatusCompleted {
		t.Fatalf("fresh task Status = %q, want %q (Done: true on turn 2)", freshTask.Status, registry.TaskStatusCompleted)
	}
}

// TestDispatch_DifferentConversation_DoesNotReuseAnOpenTask proves the
// active-task lookup is scoped by conversation, not just workspace: a
// second, unrelated conversation dispatching to the same workspace
// while an earlier one is still open must launch its own fresh task,
// not send its message into the other conversation's session.
func TestDispatch_DifferentConversation_DoesNotReuseAnOpenTask(t *testing.T) {
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "still working", Done: false}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-A", "hello"); err != nil {
		t.Fatalf("Dispatch (conv-A): %v", err)
	}
	if _, err := r.Dispatch(context.Background(), "conv-B", "hello"); err != nil {
		t.Fatalf("Dispatch (conv-B): %v", err)
	}

	if len(exec.sessions) != 2 {
		t.Fatalf("sessions = %+v, want 2 (one per conversation)", exec.sessions)
	}

	tasks, err := store.ListTasksByWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("ListTasksByWorkspace = %+v, want 2 tasks", tasks)
	}
}

// TestDispatch_ActiveTaskUnderTakeover_RefusesRatherThanDoubleLaunch
// proves a message arriving for a conversation whose task is under
// human takeover cleanly refuses (surfacing orchestrator.ErrHumanTakeover)
// instead of silently launching a second, conflicting session.
func TestDispatch_ActiveTaskUnderTakeover_RefusesRatherThanDoubleLaunch(t *testing.T) {
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "still working", Done: false}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "start"); err != nil {
		t.Fatalf("Dispatch (turn 1): %v", err)
	}
	if len(exec.sessions) != 1 {
		t.Fatalf("sessions = %+v, want exactly 1", exec.sessions)
	}

	tasks, err := store.ListTasksByWorkspace(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	task := tasks[0]

	orch := orchestrator.New(store, exec.factory(), noopDetector{})
	if err := orch.Takeover(context.Background(), task.ID); err != nil {
		t.Fatalf("Takeover: %v", err)
	}

	_, err = r.Dispatch(context.Background(), "conv-1", "keep going")
	if err == nil {
		t.Fatal("Dispatch while task is under human takeover: got nil error")
	}
	if !errors.Is(err, orchestrator.ErrHumanTakeover) {
		t.Fatalf("Dispatch err = %v, want it to wrap orchestrator.ErrHumanTakeover", err)
	}
	if len(exec.sessions) != 1 {
		t.Fatalf("sessions = %+v, want still exactly 1 (no second session launched)", exec.sessions)
	}
}

// TestDispatch_MoreThanOneActiveTaskForSameConversation_Errors defends
// findActiveTask's ambiguity guard: this shouldn't be reachable through
// Dispatch itself (which always reuses a found task rather than
// creating a second one), but if it ever is — e.g. data seeded another
// way — Dispatch must fail loudly rather than silently pick one.
func TestDispatch_MoreThanOneActiveTaskForSameConversation_Errors(t *testing.T) {
	store, _, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)

	for i := 0; i < 2; i++ {
		task := &registry.Task{
			ID:             uuid.NewString(),
			WorkspaceID:    ws.ID,
			Kind:           registry.TaskKindAgent,
			AgentType:      "claude-code",
			TmuxSession:    "loomux-" + uuid.NewString(),
			Status:         registry.TaskStatusAwaitingInput,
			ConversationID: "conv-1",
		}
		if err := store.CreateTask(context.Background(), task); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}

	_, err := r.Dispatch(context.Background(), "conv-1", "hi")
	if err == nil {
		t.Fatal("Dispatch with two ambiguous active tasks: got nil error")
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// noopDetector never signals — unused by this test beyond satisfying
// orchestrator.New's constructor, since Takeover doesn't touch it.
type noopDetector struct{}

func (noopDetector) Wait(ctx context.Context, task *registry.Task) error {
	<-ctx.Done()
	return ctx.Err()
}
