package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/targets"
)

// LOOM-111: a message over what is pasted into a pane is refused before
// anything is launched or sent, with its own class, and leaves the
// agent's task as it was.
func TestDispatch_MessageTooLarge_RefusedAndTaskKept(t *testing.T) {
	store, _, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "first reply", Done: false}, nil
	}
	ctx := context.Background()

	huge := strings.Repeat("x", targets.MaxPasteBytes+1)
	_, err := r.Dispatch(ctx, "conv-new", huge)
	if got := router.ClassifyError(err); got != registry.ErrorClassMessageTooLarge {
		t.Fatalf("fresh dispatch of %d bytes: class %q (err %v), want message_too_large", len(huge), got, err)
	}
	if tasks, _ := store.ListTasksByWorkspace(ctx, ws.ID); len(tasks) != 0 {
		t.Fatalf("tasks after a refused message = %+v, want none launched", tasks)
	}

	if _, err := r.Dispatch(ctx, "conv-1", "first message"); err != nil {
		t.Fatalf("Dispatch (first turn): %v", err)
	}
	_, err = r.Dispatch(ctx, "conv-1", huge)
	if got := router.ClassifyError(err); got != registry.ErrorClassMessageTooLarge {
		t.Fatalf("second turn of %d bytes: class %q (err %v), want message_too_large", len(huge), got, err)
	}
	if !strings.Contains(err.Error(), "file") {
		t.Errorf("error = %q, want it to suggest a file", err)
	}
	tasks, _ := store.ListTasksByWorkspace(ctx, ws.ID)
	if len(tasks) != 1 || tasks[0].Status != registry.TaskStatusAwaitingInput {
		t.Fatalf("tasks = %+v, want the one task still awaiting input", tasks)
	}
}
