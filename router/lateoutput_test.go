package router_test

import (
	"context"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

func (e *fakeExecutor) setFileExists(v bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fileExists = v
}

// lateHarness has run one turn that the agent ended early: its task is
// waiting for input, and the turn's marker is gone.
func lateHarness(t *testing.T) (*lastMessageHarness, *registry.Task) {
	t.Helper()
	h := newLastMessageHarness(t)
	h.payload = hookPayload(t, "last_assistant_message", "Started the build in the background; I'll report when it finishes.")
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "build it"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	h.exec.setFileExists(false)
	tasks, err := h.store.ListTasksByWorkspace(context.Background(), h.ws.ID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %v, %v; want one", tasks, err)
	}
	if tasks[0].Status != registry.TaskStatusAwaitingInput {
		t.Fatalf("task status = %s, want awaiting-input", tasks[0].Status)
	}
	return h, tasks[0]
}

func assistantMessages(t *testing.T, store registry.Store, conversationID string) []*registry.Message {
	t.Helper()
	msgs, err := store.ListMessagesByConversation(context.Background(), conversationID)
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	var out []*registry.Message
	for _, m := range msgs {
		if m.Role == registry.MessageRoleAssistant {
			out = append(out, m)
		}
	}
	return out
}

// The agent finishes a backgrounded job and writes a second final
// message after its turn was relayed (LOOM-121): it reaches the
// conversation as a new reply, with no user message, and the turn is
// kept in the task's transcript.
func TestRelayLateOutput_RelaysAFinishAfterTheTurnEnded(t *testing.T) {
	h, task := lateHarness(t)
	late := "The build finished: 42 tests passed."
	h.payload = hookPayload(t, "last_assistant_message", late)
	h.exec.setFileExists(true)

	if n := h.r.RelayLateOutput(context.Background()); n != 1 {
		t.Fatalf("RelayLateOutput = %d, want 1", n)
	}
	if len(h.relayed) != 2 || h.relayed[1] != late {
		t.Fatalf("relayed %q, want the late message second", h.relayed)
	}
	msgs := assistantMessages(t, h.store, "conv-1")
	if len(msgs) != 2 || msgs[1].Content != "relayed" || msgs[1].TaskID != task.ID {
		t.Fatalf("assistant messages = %+v, want a second reply for the task", msgs)
	}
	all, _ := h.store.ListMessagesByConversation(context.Background(), "conv-1")
	if len(all) != 3 {
		t.Fatalf("messages = %d, want 3 (user, reply, late reply)", len(all))
	}
	turns, err := h.store.ListTaskTurns(context.Background(), task.ID)
	if err != nil || len(turns) != 2 || turns[1].AgentMessage != late || turns[1].UserMessage != "" {
		t.Fatalf("turns = %+v, %v; want the late turn recorded with no user message", turns, err)
	}
	got, _ := h.store.GetTask(context.Background(), task.ID)
	if got.Status != registry.TaskStatusAwaitingInput {
		t.Fatalf("task status = %s, want it still awaiting input", got.Status)
	}
}

// Nothing new from the agent: nothing is relayed or logged.
func TestRelayLateOutput_NoMarkerNoMessage(t *testing.T) {
	h, _ := lateHarness(t)
	if n := h.r.RelayLateOutput(context.Background()); n != 0 {
		t.Fatalf("RelayLateOutput = %d, want 0", n)
	}
	if len(h.relayed) != 1 || len(assistantMessages(t, h.store, "conv-1")) != 1 {
		t.Fatalf("relayed %d, want only the first turn", len(h.relayed))
	}
}

// A late reply the relay model calls finished completes the task, as a
// finished turn does.
func TestRelayLateOutput_DoneCompletesTask(t *testing.T) {
	h, task := lateHarness(t)
	h.payload = hookPayload(t, "last_assistant_message", "All done: the build passed.")
	h.done = true
	h.exec.setFileExists(true)

	if n := h.r.RelayLateOutput(context.Background()); n != 1 {
		t.Fatalf("RelayLateOutput = %d, want 1", n)
	}
	got, _ := h.store.GetTask(context.Background(), task.ID)
	if got.Status != registry.TaskStatusCompleted {
		t.Fatalf("task status = %s, want completed", got.Status)
	}
}

// A turn in flight owns the pane, and its marker: the pass leaves the
// conversation alone.
func TestRelayLateOutput_SkipsConversationWithTurnInFlight(t *testing.T) {
	h, _ := lateHarness(t)
	h.payload = hookPayload(t, "last_assistant_message", "late")
	h.exec.setFileExists(true)

	unlock := router.LockConversation(h.r, "conv-1")
	n := h.r.RelayLateOutput(context.Background())
	unlock()
	if n != 0 || len(h.relayed) != 1 {
		t.Fatalf("RelayLateOutput = %d (relayed %d), want the busy conversation skipped", n, len(h.relayed))
	}
}

// A task that isn't waiting for input — taken over by a human, stopped
// at a prompt, finished — is not watched.
func TestRelayLateOutput_OnlyAwaitingInputTasks(t *testing.T) {
	for _, status := range []registry.TaskStatus{registry.TaskStatusHumanTakeover,
		registry.TaskStatusNeedsAttention, registry.TaskStatusCompleted, registry.TaskStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			h, task := lateHarness(t)
			task.Status = status
			if err := h.store.UpdateTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			h.payload = hookPayload(t, "last_assistant_message", "late")
			h.exec.setFileExists(true)
			if n := h.r.RelayLateOutput(context.Background()); n != 0 {
				t.Fatalf("RelayLateOutput = %d, want 0", n)
			}
		})
	}
}

// The late-reply hook hears of each one, with its task and reply.
func TestRelayLateOutput_CallsHook(t *testing.T) {
	h, task := lateHarness(t)
	var gotTask, gotReply string
	router.WithLateReplyHook(func(tk *registry.Task, reply string) { gotTask, gotReply = tk.ID, reply })(h.r)
	h.payload = hookPayload(t, "last_assistant_message", "finished")
	h.exec.setFileExists(true)
	h.r.RelayLateOutput(context.Background())
	if gotTask != task.ID || gotReply != "relayed" {
		t.Fatalf("hook got (%q, %q), want (%q, relayed)", gotTask, gotReply, task.ID)
	}
}
