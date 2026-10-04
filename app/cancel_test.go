package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

// LOOM-99 review: a conversation's running dispatch drives only its
// running task. Cancelling another open task of the same conversation (one
// left awaiting input in another workspace) cancels that task alone, never
// the dispatch or the task it drives.
func TestCancelTask_OnlyTheRunningTaskGoesThroughTheDispatch(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateTarget(ctx, &registry.Target{ID: "t", Name: "jet01", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	for _, ws := range []string{"x", "y"} {
		if err := store.CreateWorkspace(ctx, &registry.Workspace{ID: ws, Name: ws, Path: "/" + ws, TargetID: "t", Status: registry.WorkspaceStatusIdle}); err != nil {
			t.Fatalf("CreateWorkspace: %v", err)
		}
	}
	left := &registry.Task{ID: "A", WorkspaceID: "x", Kind: registry.TaskKindAgent, TmuxSession: "sa",
		Status: registry.TaskStatusAwaitingInput, ConversationID: "conv"}
	driven := &registry.Task{ID: "B", WorkspaceID: "y", Kind: registry.TaskKindAgent, TmuxSession: "sb",
		Status: registry.TaskStatusRunning, ConversationID: "conv"}
	for _, task := range []*registry.Task{left, driven} {
		if err := store.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}

	started := make(chan struct{})
	svc := dispatch.New(store, func(ctx context.Context, d *registry.Dispatch) (string, error) {
		close(started)
		<-ctx.Done()
		return "", context.Cause(ctx)
	})
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = svc.Shutdown(c)
	})
	var direct []string
	a := &App{store: store, dispatches: svc, cancelTaskDirect: func(ctx context.Context, id string) error {
		direct = append(direct, id)
		return nil
	}}
	d, err := svc.Submit(ctx, dispatch.Request{ConversationID: "conv", Message: "work in y"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	got, err := a.CancelTask(ctx, "A")
	if err != nil || got != "" {
		t.Fatalf("CancelTask(A) = %q, %v; want no dispatch, nil", got, err)
	}
	if len(direct) != 1 || direct[0] != "A" {
		t.Fatalf("cancelled directly: %v, want [A]", direct)
	}
	if cur, _ := svc.Get(ctx, d.ID); cur.Status != registry.DispatchStatusRunning {
		t.Fatalf("dispatch after cancelling A = %s, want still running", cur.Status)
	}

	got, err = a.CancelTask(ctx, "B")
	if err != nil || got != d.ID {
		t.Fatalf("CancelTask(B) = %q, %v; want the dispatch %s", got, err, d.ID)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done, err := svc.Wait(waitCtx, d.ID)
	if err != nil || done.Status != registry.DispatchStatusFailed || done.ErrorClass != registry.ErrorClassCancelled {
		t.Fatalf("dispatch after cancelling B = %+v, %v; want failed/cancelled", done, err)
	}
	if len(direct) != 1 {
		t.Fatalf("B was also cancelled directly: %v", direct)
	}

	if _, err := a.CancelTask(ctx, "nope"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("CancelTask(unknown) = %v, want ErrNotFound", err)
	}
	left.Status = registry.TaskStatusFailed
	if err := store.UpdateTask(ctx, left); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if _, err := a.CancelTask(ctx, "A"); !errors.Is(err, orchestrator.ErrTaskInactive) {
		t.Errorf("CancelTask(ended) = %v, want ErrTaskInactive", err)
	}
}
