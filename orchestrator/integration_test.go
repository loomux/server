package orchestrator_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/orchestrator/detectortest"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// TestIntegration_RealLocalTmux proves the orchestrator's actual wiring
// works end-to-end against a real local tmux session — not just the fake
// executor the rest of this package's tests use.
func TestIntegration_RealLocalTmux(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	target := &registry.Target{ID: uuid.NewString(), Name: "real-target-" + t.Name(), Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	ws := &registry.Workspace{
		ID:       uuid.NewString(),
		Name:     "real-ws-" + t.Name(),
		Path:     t.TempDir(), // a real directory: tmux's -c requires one to exist
		TargetID: target.ID,
		Status:   registry.WorkspaceStatusIdle,
	}
	if err := store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	detector := detectortest.NewManualDetector()
	o := orchestrator.New(store, targets.NewExecutor, detector)

	task, err := o.Launch(ctx, ws.ID, "conv-real", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	realExec := targets.NewLocalExecutor()
	t.Cleanup(func() {
		if err := realExec.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	exists, err := realExec.HasSession(ctx, task.TmuxSession)
	if err != nil {
		t.Fatalf("HasSession (verify launch): %v", err)
	}
	if !exists {
		t.Fatalf("real tmux session %q does not exist after Launch", task.TmuxSession)
	}

	marker := "integration-marker-" + task.ID
	if err := o.SendMessage(ctx, task.ID, "echo "+marker); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var captured string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		captured, err = realExec.CapturePane(ctx, task.TmuxSession)
		if err != nil {
			t.Fatalf("CapturePane: %v", err)
		}
		if strings.Contains(captured, marker) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(captured, marker) {
		t.Fatalf("pane never showed marker %q; last capture:\n%s", marker, captured)
	}

	// Real completion detection is LOOM-6's job — signal manually here,
	// same as every other test in this package.
	detector.Signal(task.ID)
	if err := o.WaitForCompletion(ctx, task.ID); err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}

	if err := o.Complete(ctx, task.ID, "integration test summary"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// The pane outlives Complete for a grace period (LOOM-91), then the
	// finished-pane sweep tears it down.
	exists, err = realExec.HasSession(ctx, task.TmuxSession)
	if err != nil {
		t.Fatalf("HasSession (verify complete): %v", err)
	}
	if !exists {
		t.Fatalf("real tmux session %q killed by Complete, want it kept for the grace period", task.TmuxSession)
	}
	if n := orchestrator.NewFinishedPaneSweeper(o, 0, nil).Sweep(ctx); n != 1 {
		t.Fatalf("finished-pane sweep reaped %d, want 1", n)
	}
	exists, err = realExec.HasSession(ctx, task.TmuxSession)
	if err != nil {
		t.Fatalf("HasSession (verify sweep): %v", err)
	}
	if exists {
		t.Fatalf("real tmux session %q still exists after the finished-pane sweep", task.TmuxSession)
	}

	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != registry.TaskStatusCompleted {
		t.Fatalf("Status = %q, want %q", stored.Status, registry.TaskStatusCompleted)
	}

	updatedWS, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if updatedWS.RollingSummary != "integration test summary" {
		t.Fatalf("RollingSummary = %q, want %q", updatedWS.RollingSummary, "integration test summary")
	}
	if updatedWS.Status != registry.WorkspaceStatusIdle {
		t.Fatalf("workspace Status = %q, want %q", updatedWS.Status, registry.WorkspaceStatusIdle)
	}
}
