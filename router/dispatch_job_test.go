package router_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// createDispatchRow stores a dispatch row so messages can reference it.
func createDispatchRow(t *testing.T, store registry.Store, id, conversationID string, userMessage *registry.Message) {
	t.Helper()
	if err := store.CreateDispatch(context.Background(), &registry.Dispatch{
		ID: id, ConversationID: conversationID, Message: "m", RequestHash: "h",
	}, userMessage); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
}

// LOOM-80: messages written during a dispatch job carry its id.
func TestDispatch_WithDispatchID_StampsMessages(t *testing.T) {
	store, _, r, model := setup(t)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "the answer"}, nil
	}
	createDispatchRow(t, store, "d-1", "conv-1", nil)

	if _, err := r.Dispatch(context.Background(), "conv-1", "what's up", router.WithDispatchID("d-1")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	msgs, _ := store.ListMessagesByConversation(context.Background(), "conv-1")
	if len(msgs) != 2 || msgs[0].DispatchID != "d-1" || msgs[1].DispatchID != "d-1" {
		t.Fatalf("messages = %+v, want both stamped with d-1", msgs)
	}
}

// LOOM-80: the job already stored the user message at submit, so the
// router writes only the reply.
func TestDispatch_UserMessageLogged_WritesOnlyReply(t *testing.T) {
	h := newRouterHarness(t)
	createDispatchRow(t, h.store, "d-2", "conv-2", &registry.Message{
		ID: uuid.NewString(), ConversationID: "conv-2", Role: registry.MessageRoleUser, Content: "do it",
	})

	if _, err := h.r.Dispatch(context.Background(), "conv-2", "do it",
		router.WithDispatchID("d-2"), router.WithUserMessageLogged()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	msgs, _ := h.store.ListMessagesByConversation(context.Background(), "conv-2")
	if len(msgs) != 2 {
		t.Fatalf("messages = %+v, want the submitted user message plus one reply", msgs)
	}
	if msgs[1].Role != registry.MessageRoleAssistant || msgs[1].Content != "ok" || msgs[1].DispatchID != "d-2" || msgs[1].TaskID == "" {
		t.Fatalf("reply = %+v", msgs[1])
	}
}

// LOOM-80: a turn cut off by server shutdown is not the task's failure —
// the agent may still finish, and LOOM-82 picks the task back up — so it
// stays running instead of being failed as wait_failed.
func TestDispatch_ShutdownCancellation_LeavesTaskRunning(t *testing.T) {
	h := newRouterHarness(t)
	h.exec.captureFunc = func() string { return uuid.NewString() }
	ctx, cancel := context.WithCancelCause(context.Background())
	h.exec.onSendKeys = func() { time.AfterFunc(100*time.Millisecond, func() { cancel(orchestrator.ErrInterrupted) }) }

	if _, err := h.r.Dispatch(ctx, "conv-1", "go"); err == nil {
		t.Fatal("Dispatch succeeded, want the cancellation error")
	}
	task := onlyTask(t, h.store, h.ws.ID)
	if task.Status != registry.TaskStatusRunning || task.ErrorClass != "" {
		t.Fatalf("task after shutdown cancellation = %+v, want still running", task)
	}
}

func TestClassifyError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want registry.ErrorClass
	}{
		{errors.New("whatever"), registry.ErrorClassInternal},
		{context.Canceled, registry.ErrorClassWaitFailed},
		{&orchestrator.TurnTimeoutError{Reason: orchestrator.TimeoutMaxTurnDuration, Limit: time.Hour}, registry.ErrorClassTimeout},
		{&orchestrator.ProcessExitedError{}, registry.ErrorClassAgentExited},
	} {
		if got := router.ClassifyError(tc.err); got != tc.want {
			t.Errorf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
