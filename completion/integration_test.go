package completion_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// TestIntegration_IdleWatcherRealLocalTmux proves the idle heuristic
// (tier 3) actually fires against a real local tmux pane, not just the
// scripted fake from idle_test.go. The pane runs a plain `sh` command
// that echoes once then sleeps — deliberately not the user's own
// interactive shell, whose prompt theme could keep redrawing and
// destabilize the pane, defeating the point of this test.
func TestIntegration_IdleWatcherRealLocalTmux(t *testing.T) {
	exec := targets.NewLocalExecutor()
	t.Cleanup(func() {
		if err := exec.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	ctx := context.Background()
	session := "loomux-completion-test-" + uuid.NewString()

	if err := exec.NewSession(ctx, session, t.TempDir(), "sh -c 'echo idle-test-marker; sleep 30'"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.KillSession(context.Background(), session)
	})

	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	pollInterval := 50 * time.Millisecond
	idleTimeout := 300 * time.Millisecond
	w := completion.NewIdleWatcher(factory, pollInterval)

	task := &registry.Task{ID: "integration-task", TmuxSession: session}
	target := &registry.Target{ID: "integration-target", Kind: registry.TargetKindLocal}

	start := time.Now()
	errCh := make(chan error, 1)
	go func() { errCh <- w.Wait(ctx, task, target, idleTimeout) }()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("idle heuristic never fired within 5s")
	}

	if elapsed := time.Since(start); elapsed < idleTimeout {
		t.Fatalf("Wait returned after %v, want at least the configured idle window %v", elapsed, idleTimeout)
	}
}
