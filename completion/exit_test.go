package completion_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// exitingExecutor reports its pane's process as exited (with exit) from
// the exitAfter-th PaneExited call onwards, and running before that. Its
// pane output never changes and no marker file ever appears, so neither
// the idle nor the marker tier could complete on its own within a test's
// timeout — only the process exiting can end a Wait.
type exitingExecutor struct {
	scriptedExecutor
	mu         sync.Mutex
	exitAfter  int
	exitCalls  int
	exit       *targets.PaneExit
	captureHit bool
}

func newExitingExecutor(exitAfter int, exit *targets.PaneExit) *exitingExecutor {
	return &exitingExecutor{
		scriptedExecutor: *newScriptedExecutor([]string{"steady output"}),
		exitAfter:        exitAfter,
		exit:             exit,
	}
}

func (e *exitingExecutor) PaneExited(context.Context, string) (*targets.PaneExit, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.exitCalls++
	if e.exitCalls >= e.exitAfter {
		return e.exit, nil
	}
	return nil, nil
}

func (e *exitingExecutor) CapturePane(ctx context.Context, target string) (string, error) {
	e.mu.Lock()
	e.captureHit = true
	e.mu.Unlock()
	return e.scriptedExecutor.CapturePane(ctx, target)
}

func waitWithTimeout(t *testing.T, d *completion.Detector, task *registry.Task) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Wait(ctx, task) }()
	select {
	case err := <-errCh:
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("Wait only returned on its timeout; the pane's process exiting should have ended it")
		}
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Wait never returned")
		return nil
	}
}

// A command task (LOOM-71/72) completes exactly when its process exits —
// TierExit — and is never judged by its output going quiet.
func TestDetector_CommandTaskCompletesOnProcessExit(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindCommand, TmuxSession: "sess"}

	exec := newExitingExecutor(3, &targets.PaneExit{Status: 1, Output: "boom"})
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	// An idle timeout of zero would make any output-quiet heuristic
	// "complete" on its second poll; TierExit must not consult it at all.
	d := completion.NewDetector(store, factory, completion.Config{"": {Tier: completion.TierIdle, IdleTimeout: time.Nanosecond}}, t.TempDir())

	if err := waitWithTimeout(t, d, task); err != nil {
		t.Fatalf("Wait: %v (a command's exit — even a non-zero one — is its completion, not an error)", err)
	}
	if exec.exitCalls < 3 {
		t.Errorf("Wait returned after %d exit checks, before the process exited", exec.exitCalls)
	}
	if exec.captureHit {
		t.Error("TierExit consulted the pane's output; completion must be the process exiting only")
	}
}

// An interactive agent (marker tier) or idle-tier shell whose process
// exits has nothing left to signal completion with: Wait must return a
// *orchestrator.ProcessExitedError carrying the exit status and final
// output at once, instead of waiting for a marker that will never be
// written (or an idle timeout) — the LOOM-71 "codex: command not found"
// case.
func TestDetector_ProcessExitEndsWaitForEveryTier(t *testing.T) {
	cases := []struct {
		name string
		cfg  completion.Config
		task registry.Task
	}{
		{"marker", completion.Config{"codex": {Tier: completion.TierMarker}},
			registry.Task{Kind: registry.TaskKindAgent, AgentType: "codex"}},
		{"idle", completion.Config{"": {Tier: completion.TierIdle, IdleTimeout: time.Hour}},
			registry.Task{Kind: registry.TaskKindShell}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
			task := tc.task
			task.ID, task.WorkspaceID, task.TmuxSession = uuid.NewString(), ws.ID, "sess"

			exec := newExitingExecutor(2, &targets.PaneExit{Status: 127, Output: "sh: 1: codex: not found"})
			factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
			d := completion.NewDetector(store, factory, tc.cfg, t.TempDir())

			err := waitWithTimeout(t, d, &task)
			var exited *orchestrator.ProcessExitedError
			if !errors.As(err, &exited) {
				t.Fatalf("Wait = %v, want *orchestrator.ProcessExitedError", err)
			}
			if exited.Status != 127 || exited.Output != "sh: 1: codex: not found" {
				t.Errorf("ProcessExitedError = %+v, want status 127 and the pane's output", exited)
			}
		})
	}
}
