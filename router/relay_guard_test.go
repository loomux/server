package router_test

import (
	"context"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// A turn whose relayed reply asks the user something isn't finished,
// whatever the relay model said: tearing its pane down would leave the
// answer nowhere to go. (2026-10-04: the relay marked 'Asked: "Should I
// create hello.txt?" and am waiting for your answer.' as done.)
func TestDispatch_ReplyAskingUser_KeepsTaskOpen(t *testing.T) {
	for _, reply := range []string{
		`Asked: "Should I create hello.txt?" and am waiting for your answer.`,
		"Drafted the migration. Should I also update the tests?",
		"I'm waiting for your confirmation before deleting the old table.",
	} {
		t.Run(reply[:12], func(t *testing.T) {
			h := newRouterHarness(t)
			h.model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
				return router.RelayResult{Reply: reply, Done: true}, nil
			}
			if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if task := onlyTask(t, h.store, h.ws.ID); task.Status != registry.TaskStatusAwaitingInput {
				t.Errorf("task = %s, want awaiting input: the reply asks the user something", task.Status)
			}
		})
	}
}

func TestDispatch_FinishedReply_Completes(t *testing.T) {
	h := newRouterHarness(t)
	h.model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "Created hello.txt with the greeting. Done.", Done: true}, nil
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if task := onlyTask(t, h.store, h.ws.ID); task.Status != registry.TaskStatusCompleted {
		t.Errorf("task = %s, want completed", task.Status)
	}
}
