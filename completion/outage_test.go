package completion_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// flakyMarkerExecutor's FileExists fails as unreachable for its first
// failures calls (all of them when failures < 0), then reports the
// marker present.
type flakyMarkerExecutor struct {
	scriptedExecutor
	mu       sync.Mutex
	failures int
	calls    int
}

func (e *flakyMarkerExecutor) FileExists(context.Context, string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.failures < 0 || e.calls <= e.failures {
		return false, targets.ErrUnreachable
	}
	return true, nil
}
func (e *flakyMarkerExecutor) PaneExited(context.Context, string) (*targets.PaneExit, error) {
	return nil, nil
}

func waitMarker(t *testing.T, exec *flakyMarkerExecutor) error {
	t.Helper()
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s"}
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	w := completion.NewMarkerWatcher(factory, t.TempDir(), 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target, _ := store.GetTarget(ctx, ws.TargetID)
	return w.Wait(ctx, task, target)
}

// A few unreachable polls (a broken ssh master, a sidecar restart) don't
// fail the turn.
func TestMarkerWait_RidesOutUnreachable(t *testing.T) {
	defer completion.SetUnreachableGrace(2 * time.Second)()
	exec := &flakyMarkerExecutor{scriptedExecutor: *newScriptedExecutor([]string{""}), failures: 3}
	if err := waitMarker(t, exec); err != nil {
		t.Errorf("Wait = %v, want the marker found once the target answers again", err)
	}
}

// A target unreachable for longer than the grace fails the wait.
func TestMarkerWait_GivesUpAfterGrace(t *testing.T) {
	defer completion.SetUnreachableGrace(100 * time.Millisecond)()
	exec := &flakyMarkerExecutor{scriptedExecutor: *newScriptedExecutor([]string{""}), failures: -1}
	if err := waitMarker(t, exec); !errors.Is(err, targets.ErrUnreachable) {
		t.Errorf("Wait = %v, want ErrUnreachable after the grace", err)
	}
}
