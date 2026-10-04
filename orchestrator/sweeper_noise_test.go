package orchestrator_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// LOOM-122: a completed task's pane in its grace period belongs to the
// finished-pane sweep; the orphan sweep neither reports nor kills it.
func TestOrphanSweeper_LeavesKeptPanesAlone(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()
	task, err := o.Launch(ctx, ws.ID, "c", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := o.Complete(ctx, task.ID, "done"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	exec.runOnceOut = fmt.Sprintf("%s %d\n", task.TmuxSession, time.Now().Add(-48*time.Hour).Unix())

	var logs bytes.Buffer
	sweeper := orchestrator.NewOrphanSweeper(o, 24*time.Hour, slog.New(slog.NewTextHandler(&logs, nil)))
	if killed := sweeper.Sweep(ctx); killed != 0 {
		t.Errorf("killed %d, want the kept pane left alone", killed)
	}
	if strings.Contains(logs.String(), "orphan") {
		t.Errorf("kept pane reported as an orphan:\n%s", logs.String())
	}
	if s := exec.sessionFor(task.TmuxSession); s == nil || !s.alive {
		t.Errorf("kept pane killed")
	}
	_ = store
}

// LOOM-122: a pane the finished-pane sweep can't tear down (its target
// down) is warned about once, not on every one-minute sweep.
func TestFinishedPaneSweeper_WarnsOncePerPane(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()
	task, err := o.Launch(ctx, ws.ID, "c", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := o.Complete(ctx, task.ID, "done"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	stored, _ := store.GetTask(ctx, task.ID)
	past := time.Now().UTC().Add(-time.Hour)
	stored.CompletedAt = &past
	if err := store.UpdateTask(ctx, stored); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	exec.unreachable = true

	var logs bytes.Buffer
	sweeper := orchestrator.NewFinishedPaneSweeper(o, 15*time.Minute, slog.New(slog.NewTextHandler(&logs, nil)))
	for range 3 {
		sweeper.Sweep(ctx)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings over 3 sweeps, want 1:\n%s", n, logs.String())
	}

	// Once it's back and the pane is torn down, a later failure warns again.
	exec.unreachable = false
	if n := sweeper.Sweep(ctx); n != 1 {
		t.Fatalf("Sweep after the target came back = %d, want 1", n)
	}
}
