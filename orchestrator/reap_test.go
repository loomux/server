package orchestrator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

func TestReap_KillsSessionAndSetsReapedAt(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := o.Reap(ctx, task.ID); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	// Status is deliberately unchanged — a reaped task is still logically
	// open; only its session is gone.
	if stored.Status != registry.TaskStatusRunning {
		t.Fatalf("Status = %q, want unchanged %q", stored.Status, registry.TaskStatusRunning)
	}
	if stored.ReapedAt == nil {
		t.Fatal("ReapedAt is nil, want set")
	}

	exists, err := exec.HasSession(ctx, task.TmuxSession)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if exists {
		t.Fatal("session still exists after Reap, want torn down")
	}

	updatedWS, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if updatedWS.Status != registry.WorkspaceStatusActive {
		t.Fatalf("workspace Status = %q, want unchanged %q (reaping only tears down the session, nothing else structural)", updatedWS.Status, registry.WorkspaceStatusActive)
	}
}

func TestReap_AlreadyGoneSession_IsIdempotent(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// Simulate the session having already vanished some other way (crash,
	// manual kill) before the reaper got to it.
	if err := exec.KillSession(ctx, task.TmuxSession); err != nil {
		t.Fatalf("KillSession (setup): %v", err)
	}

	if err := o.Reap(ctx, task.ID); err != nil {
		t.Fatalf("Reap on an already-gone session: %v, want no error", err)
	}

	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.ReapedAt == nil {
		t.Fatal("ReapedAt is nil, want set even though the session was already gone")
	}
}

func TestReap_UnknownTask_ReturnsNotFound(t *testing.T) {
	_, _, _, _, o := setup(t)
	err := o.Reap(context.Background(), "does-not-exist")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Reap on unknown task: err = %v, want ErrNotFound", err)
	}
}

// TestReaper_Sweep_ReapsOnlyIdleNonTerminalTasks covers the sweep's
// filtering: only an AwaitingInput task idle past threshold gets reaped —
// a task within threshold, a terminal (Completed) task, a HumanTakeover
// task, and a Running task (however old, mid-turn is not idle) all do
// not — real time passage (small threshold + sleep), not backdated
// timestamps, matching this repo's existing fast-test conventions (e.g.
// router's shortIdleAgentTypes).
func TestReaper_Sweep_ReapsOnlyIdleNonTerminalTasks(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()

	idleAwaiting, err := o.Launch(ctx, ws.ID, "conv-idle", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch (idleAwaiting): %v", err)
	}
	idleAwaiting.Status = registry.TaskStatusAwaitingInput
	if err := store.UpdateTask(ctx, idleAwaiting); err != nil {
		t.Fatalf("UpdateTask (idleAwaiting): %v", err)
	}

	idleTakeover, err := o.Launch(ctx, ws.ID, "conv-takeover", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch (idleTakeover): %v", err)
	}
	if err := o.Takeover(ctx, idleTakeover.ID); err != nil {
		t.Fatalf("Takeover: %v", err)
	}

	idleCompleted, err := o.Launch(ctx, ws.ID, "conv-completed", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch (idleCompleted): %v", err)
	}
	if err := o.Complete(ctx, idleCompleted.ID, "done"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Left Running (never sent a message, never completed) and allowed to
	// age past threshold just like the three above — this is the case a
	// real end-to-end test caught: a Running task means a turn is
	// actively in flight, however long it takes, not "idle waiting for a
	// follow-up." Must never be reaped regardless of how old UpdatedAt
	// gets.
	longRunning, err := o.Launch(ctx, ws.ID, "conv-long-running", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch (longRunning): %v", err)
	}

	threshold := 50 * time.Millisecond
	time.Sleep(threshold + 30*time.Millisecond) // let the four above go idle past threshold

	// Launched right before sweeping — well within threshold, must not
	// be reaped.
	freshRunning, err := o.Launch(ctx, ws.ID, "conv-fresh", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch (freshRunning): %v", err)
	}

	reaper := orchestrator.NewReaper(o, threshold, orchestrator.WithReaperLogger(func(string, ...any) {}))
	reaper.Sweep(ctx)

	assertReaped := func(id string, want bool) {
		t.Helper()
		got, err := store.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", id, err)
		}
		if (got.ReapedAt != nil) != want {
			t.Fatalf("task %s ReapedAt = %v, want set=%v", id, got.ReapedAt, want)
		}
	}
	assertReaped(idleAwaiting.ID, true)
	assertReaped(freshRunning.ID, false)
	assertReaped(idleTakeover.ID, false)  // human-takeover is deliberately excluded — see Reaper doc comment
	assertReaped(idleCompleted.ID, false) // terminal, nothing to reap
	assertReaped(longRunning.ID, false)   // Running: a turn is in flight, not idle — see Reaper doc comment

	exists, err := exec.HasSession(ctx, idleAwaiting.TmuxSession)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if exists {
		t.Fatal("idleAwaiting session still exists after sweep, want torn down")
	}
}

// TestReaper_Run_SweepsOnTickerUntilCancelled proves the ticker-driven
// loop itself works end-to-end (not just Sweep called directly).
func TestReaper_Run_SweepsOnTickerUntilCancelled(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	task.Status = registry.TaskStatusAwaitingInput
	if err := store.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // clear the tiny threshold below

	reaper := orchestrator.NewReaper(o, 10*time.Millisecond, orchestrator.WithReaperLogger(func(string, ...any) {}))
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		reaper.Run(runCtx, 10*time.Millisecond)
	}()

	deadline := time.After(2 * time.Second)
	for {
		exists, err := exec.HasSession(ctx, task.TmuxSession)
		if err != nil {
			t.Fatalf("HasSession: %v", err)
		}
		if !exists {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run never reaped the idle task within the deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
}
