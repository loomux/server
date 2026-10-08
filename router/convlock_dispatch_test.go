package router_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/router"
)

// LOOM-165: a dispatch waiting behind another turn of its conversation
// gives up when its context ends, without routing anything, rather than
// block until that turn finishes.
func TestDispatch_WaitingForConversationHonoursContext(t *testing.T) {
	_, _, r, model := setup(t)
	var decided atomic.Bool
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		decided.Store(true)
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "hi"}, nil
	}
	unlock := router.LockConversation(r, "conv-held")
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.Dispatch(ctx, "conv-held", "hello")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dispatch = %v, want DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Dispatch still waiting on the held conversation 5s after its context ended")
	}
	if decided.Load() {
		t.Error("the turn was routed although it never got the conversation")
	}
}
