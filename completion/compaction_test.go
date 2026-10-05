package completion_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// cyclingExecutor's pane shows screens[i] on the i-th capture, the last
// one repeating; no marker, no exit.
type cyclingExecutor struct {
	scriptedExecutor
	mu      sync.Mutex
	screens []string
	n       int
}

func (e *cyclingExecutor) CapturePane(context.Context, string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	i := e.n
	if i >= len(e.screens) {
		i = len(e.screens) - 1
	}
	e.n++
	return e.screens[i], nil
}
func (e *cyclingExecutor) PaneExited(context.Context, string) (*targets.PaneExit, error) {
	return nil, nil
}

func compactionDetector(t *testing.T, screens []string) (*completion.Detector, *registry.Task) {
	t.Helper()
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s"}
	exec := &cyclingExecutor{scriptedExecutor: *newScriptedExecutor([]string{""}), screens: screens}
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	compacting := func(screen string) bool { return strings.HasPrefix(screen, "compacting") }
	d := completion.NewDetector(store, factory, completion.Config{"agent": {
		Tier: completion.TierMarker, MaxTurnDuration: 2 * time.Second, NoProgressTimeout: time.Hour, DetectCompaction: compacting,
	}}, t.TempDir(), completion.WithProgressPollInterval(5*time.Millisecond))
	return d, task
}

// LOOM-109: an agent that starts compacting CompactionLoopLimit times in
// one turn ends the wait with a CompactionLoopError. A compaction seen on
// several checks in a row counts once.
func TestDetector_CompactionLoop(t *testing.T) {
	var screens []string
	for i := 0; i < completion.CompactionLoopLimit; i++ {
		screens = append(screens, fmt.Sprintf("working %d", i), "compacting", "compacting", "compacting")
	}
	screens = append(screens, "working after")
	d, task := compactionDetector(t, screens)
	err := d.Wait(context.Background(), task)
	var loop *orchestrator.CompactionLoopError
	if !errors.As(err, &loop) {
		t.Fatalf("Wait = %v, want *orchestrator.CompactionLoopError", err)
	}
	if loop.Count != completion.CompactionLoopLimit {
		t.Errorf("Count = %d, want %d", loop.Count, completion.CompactionLoopLimit)
	}
}

// Fewer compactions than the limit, however long each shows, are a turn
// doing its work: it runs to its own bound.
func TestDetector_CompactionBelowLimit(t *testing.T) {
	var screens []string
	for i := 0; i < completion.CompactionLoopLimit-1; i++ {
		screens = append(screens, fmt.Sprintf("working %d", i), "compacting", "compacting")
	}
	screens = append(screens, "working after")
	d, task := compactionDetector(t, screens)
	err := d.Wait(context.Background(), task)
	var timeout *orchestrator.TurnTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("Wait = %v, want the turn's own time bound", err)
	}
}
