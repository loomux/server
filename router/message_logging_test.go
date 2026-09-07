package router_test

import (
	"context"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

func TestDispatch_AnswerDirectly_LogsMessagePair(t *testing.T) {
	store, _, r, model := setup(t)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "the answer"}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "what's up"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	msgs, err := store.ListMessagesByConversation(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != registry.MessageRoleUser || msgs[0].Content != "what's up" {
		t.Fatalf("msgs[0] = %+v, want role=user content=%q", msgs[0], "what's up")
	}
	if msgs[1].Role != registry.MessageRoleAssistant || msgs[1].Content != "the answer" {
		t.Fatalf("msgs[1] = %+v, want role=assistant content=%q", msgs[1], "the answer")
	}
	if msgs[0].TaskID != "" || msgs[1].TaskID != "" {
		t.Fatalf("msgs = %+v, want empty TaskID for an answer_directly turn", msgs)
	}
}

func TestDispatch_UseWorkspace_LogsMessagePairWithTaskID(t *testing.T) {
	store, _, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "condensed reply", Done: true}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "do the thing"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	tasks, err := store.ListTasksByWorkspace(context.Background(), ws.ID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("ListTasksByWorkspace: tasks=%+v err=%v", tasks, err)
	}
	taskID := tasks[0].ID

	msgs, err := store.ListMessagesByConversation(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].TaskID != taskID || msgs[1].TaskID != taskID {
		t.Fatalf("msgs = %+v, want TaskID = %q", msgs, taskID)
	}
	if msgs[0].Content != "do the thing" || msgs[1].Content != "condensed reply" {
		t.Fatalf("msgs content = [%q, %q], want [%q, %q]", msgs[0].Content, msgs[1].Content, "do the thing", "condensed reply")
	}
}

func TestDispatch_SecondTurnSameTask_AppendsMoreMessages(t *testing.T) {
	store, _, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	reply := "first reply"
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: reply, Done: false}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "first message"); err != nil {
		t.Fatalf("Dispatch (first turn): %v", err)
	}
	reply = "second reply"
	if _, err := r.Dispatch(context.Background(), "conv-1", "second message"); err != nil {
		t.Fatalf("Dispatch (second turn): %v", err)
	}

	msgs, err := store.ListMessagesByConversation(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4 (two turns)", len(msgs))
	}
	wantContent := []string{"first message", "first reply", "second message", "second reply"}
	for i, want := range wantContent {
		if msgs[i].Content != want {
			t.Fatalf("msgs[%d].Content = %q, want %q (full order = %+v)", i, msgs[i].Content, want, msgs)
		}
	}
}

func TestDispatch_Failure_LogsNothing(t *testing.T) {
	store, _, r, model := setup(t)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: "does-not-exist", AgentType: "claude-code"}, nil
	}

	if _, err := r.Dispatch(context.Background(), "conv-1", "hi"); err == nil {
		t.Fatal("Dispatch against an unknown workspace: got nil error")
	}

	msgs, err := store.ListMessagesByConversation(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("ListMessagesByConversation = %+v, want no messages logged for a failed dispatch", msgs)
	}
}
