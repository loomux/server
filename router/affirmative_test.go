package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// A bare affirmative with no offer waiting for it starts nothing,
// whatever the routing model read into it: the conversation is asked
// what it wants instead.
func TestDispatch_BareAffirmative_NoOffer_AsksFirst(t *testing.T) {
	for _, message := range []string{"yes", "ok", "go", "do it", "Sure, go ahead!", "yes please"} {
		t.Run(message, func(t *testing.T) {
			h := newRouterHarness(t)
			// The router's own earlier answer ended in a question: not an
			// offer.
			if err := h.store.CreateMessage(context.Background(), &registry.Message{ID: uuid.NewString(),
				ConversationID: "conv-1", Role: registry.MessageRoleAssistant,
				Content: "That machine is unusable. If you'd like to run it elsewhere, let me know!"}); err != nil {
				t.Fatalf("CreateMessage: %v", err)
			}
			reply, err := h.r.Dispatch(context.Background(), "conv-1", message)
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if !strings.Contains(reply, "haven't started anything") {
				t.Errorf("reply = %q, want a question instead", reply)
			}
			assertNothingStarted(t, h, h.ws.ID)
		})
	}
}

// The same holds in a conversation that has worked before, with nothing
// open: its last workspace isn't an offer either.
func TestDispatch_BareAffirmative_FinishedWork_AsksFirst(t *testing.T) {
	h := newRouterHarness(t)
	h.model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "done", Done: true}, nil
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "write the migration"); err != nil {
		t.Fatalf("Dispatch 1: %v", err)
	}
	reply, err := h.r.Dispatch(context.Background(), "conv-1", "yes")
	if err != nil {
		t.Fatalf("Dispatch 2: %v", err)
	}
	if !strings.Contains(reply, "haven't started anything") {
		t.Errorf("reply = %q, want a question instead", reply)
	}
	if n := len(h.exec.launchedCommands()); n != 1 {
		t.Errorf("launched %d, want only the first turn's", n)
	}
}

// With a task open, a bare affirmative the routing model sends to a
// workspace this conversation never used is asked about; the open task
// isn't touched.
func TestDispatch_BareAffirmative_UnusedWorkspace_AsksFirst(t *testing.T) {
	h, task := openTaskHarness(t)
	other := &registry.Workspace{ID: uuid.NewString(), Name: "someone-elses", TargetID: h.ws.TargetID,
		Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(context.Background(), other); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: other.ID, AgentType: "claude-code",
			LeaveOpenTask: true}, nil
	}
	launched := len(h.exec.launchedCommands())
	sent := len(h.exec.sentKeys())
	reply, err := h.r.Dispatch(context.Background(), "conv-1", "yes")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(reply, "someone-elses") || !strings.Contains(reply, "haven't started anything") {
		t.Errorf("reply = %q, want a question naming the workspace", reply)
	}
	assertNothingStarted(t, h, other.ID)
	if n := len(h.exec.launchedCommands()); n != launched {
		t.Errorf("launched %d, want %d", n, launched)
	}
	if n := len(h.exec.sentKeys()); n != sent {
		t.Errorf("sent keys %d times, want %d: the open task got the yes", n, sent)
	}
	if got := onlyTask(t, h.store, h.ws.ID); got.ID != task.ID || got.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("open task = %+v, want %s still awaiting input", got, task.ID)
	}
}

// A bare affirmative answering the open task's question goes to it.
func TestDispatch_BareAffirmative_OpenTask_Continues(t *testing.T) {
	h, task := openTaskHarness(t)
	sent := len(h.exec.sentKeys())
	reply, err := h.r.Dispatch(context.Background(), "conv-1", "yes")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if strings.Contains(reply, "haven't started anything") {
		t.Errorf("reply = %q, want the open task continued", reply)
	}
	if n := len(h.exec.sentKeys()); n <= sent {
		t.Errorf("sent keys %d times, want the yes sent to the open task", n)
	}
	if got := onlyTask(t, h.store, h.ws.ID); got.ID != task.ID {
		t.Errorf("task = %s, want the open task %s", got.ID, task.ID)
	}
}

// A recorded offer's "yes" still runs what it offered.
func TestDispatch_BareAffirmative_PendingOffer_Runs(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	ws := h.policyWorkspace(t)
	h.setPolicy(t, registry.TargetPolicy{RequireConfirmation: true})
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})
	ctx := context.Background()
	if _, err := h.r.Dispatch(ctx, "conv-1", "run the tests"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if n := len(h.exec.launchedCommands()); n != 0 {
		t.Fatalf("launched %d before the confirmation, want 0", n)
	}
	reply, err := h.r.Dispatch(ctx, "conv-1", "yes")
	if err != nil {
		t.Fatalf("Dispatch(yes): %v", err)
	}
	if strings.Contains(reply, "haven't started anything") {
		t.Errorf("reply = %q, want the offer carried out", reply)
	}
	if n := len(h.exec.launchedCommands()); n != 1 {
		t.Errorf("launched %d after the confirmation, want 1", n)
	}
}

func assertNothingStarted(t *testing.T, h *routerHarness, workspaceID string) {
	t.Helper()
	if tasks, err := h.store.ListTasksByWorkspace(context.Background(), workspaceID); err != nil || len(tasks) != 0 {
		t.Errorf("tasks in %s = %+v, %v; want none", workspaceID, tasks, err)
	}
	if n := len(h.exec.launchedCommands()); n != 0 && workspaceID == h.ws.ID {
		t.Errorf("launched %d, want none", n)
	}
}

// sentKeys lists the text sent to every session.
func (e *fakeExecutor) sentKeys() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, s := range e.sessions {
		out = append(out, s.keys...)
	}
	return out
}
