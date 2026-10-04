package orchestrator_test

import (
	"context"
	"testing"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// LOOM-91: a completed task's pane is kept for a grace period, then torn
// down and the task marked reaped. Panes still in their grace period, and
// tasks that aren't completed, are left alone.
func TestFinishedPaneSweeper(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()

	launch := func() *registry.Task {
		t.Helper()
		task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "claude")
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		return task
	}
	completeAt := func(task *registry.Task, at time.Time) {
		t.Helper()
		if err := o.Complete(ctx, task.ID, "done"); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		stored, err := store.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		stored.CompletedAt = &at
		if err := store.UpdateTask(ctx, stored); err != nil {
			t.Fatalf("UpdateTask: %v", err)
		}
	}
	sweeper := orchestrator.NewFinishedPaneSweeper(o, 15*time.Minute, nil)
	now := time.Now().UTC()
	expired := launch()
	completeAt(expired, now.Add(-20*time.Minute))
	inGrace := launch()
	completeAt(inGrace, now.Add(-5*time.Minute))
	ancient := launch() // finished long ago: the orphan sweep's business
	completeAt(ancient, now.Add(-48*time.Hour))
	open := launch()

	if n := sweeper.Sweep(ctx); n != 1 {
		t.Errorf("Sweep = %d, want 1", n)
	}
	for _, tc := range []struct {
		task       *registry.Task
		live, reap bool
	}{
		{expired, false, true},
		{inGrace, true, false},
		{ancient, true, false},
		{open, true, false},
	} {
		live, err := exec.HasSession(ctx, tc.task.TmuxSession)
		if err != nil {
			t.Fatalf("HasSession: %v", err)
		}
		stored, err := store.GetTask(ctx, tc.task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if live != tc.live || (stored.ReapedAt != nil) != tc.reap {
			t.Errorf("task %s: live %v reaped %v, want live %v reaped %v", tc.task.ID, live, stored.ReapedAt != nil, tc.live, tc.reap)
		}
	}

	// Already reaped: not touched again.
	if n := sweeper.Sweep(ctx); n != 0 {
		t.Errorf("second Sweep = %d, want 0", n)
	}
}
