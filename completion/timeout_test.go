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

// stuckExecutor never writes a marker and its process never exits; its
// pane either never changes (blocked on a prompt) or always does (busy).
type stuckExecutor struct {
	scriptedExecutor
	mu    sync.Mutex
	busy  bool
	ticks int
}

func (e *stuckExecutor) CapturePane(context.Context, string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.busy {
		e.ticks++
		return "working " + time.Now().String(), nil
	}
	return "Allow this command? [y/n]", nil
}
func (e *stuckExecutor) PaneExited(context.Context, string) (*targets.PaneExit, error) {
	return nil, nil
}

func waitTimedOut(t *testing.T, d *completion.Detector, task *registry.Task, within time.Duration) *orchestrator.TurnTimeoutError {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	err := d.Wait(ctx, task)
	var timeout *orchestrator.TurnTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("Wait = %v, want *orchestrator.TurnTimeoutError", err)
	}
	if elapsed := time.Since(start); elapsed > within {
		t.Errorf("Wait took %s, want the turn bounded within %s", elapsed, within)
	}
	return timeout
}

// LOOM-76: a turn whose agent never signals completion ends at the
// agent-type's MaxTurnDuration with a typed timeout — instead of waiting
// for as long as the request stays open.
func TestDetector_MaxTurnDuration(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s"}
	exec := &stuckExecutor{scriptedExecutor: *newScriptedExecutor([]string{""}), busy: true}
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	d := completion.NewDetector(store, factory, completion.Config{"agent": {
		Tier: completion.TierMarker, MaxTurnDuration: 300 * time.Millisecond, NoProgressTimeout: time.Hour,
	}}, t.TempDir(), completion.WithProgressPollInterval(20*time.Millisecond))

	timeout := waitTimedOut(t, d, task, 3*time.Second)
	if timeout.Reason != orchestrator.TimeoutMaxTurnDuration || timeout.Limit != 300*time.Millisecond {
		t.Errorf("timeout = %+v, want the max-turn-duration limit", timeout)
	}
}

// A marker-tier agent whose pane hasn't changed for NoProgressTimeout is
// taken to be blocked (an approval prompt, say) and the turn ends there.
func TestDetector_NoProgressTimeout(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s"}
	exec := &stuckExecutor{scriptedExecutor: *newScriptedExecutor([]string{""})}
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	d := completion.NewDetector(store, factory, completion.Config{"agent": {
		Tier: completion.TierMarker, MaxTurnDuration: time.Hour, NoProgressTimeout: 300 * time.Millisecond,
	}}, t.TempDir(), completion.WithProgressPollInterval(20*time.Millisecond))

	timeout := waitTimedOut(t, d, task, 3*time.Second)
	if timeout.Reason != orchestrator.TimeoutNoProgress {
		t.Errorf("timeout = %+v, want no progress", timeout)
	}
}

// A busy pane is progress: no-progress never fires while output changes.
func TestDetector_NoProgressTimeout_BusyPaneIsProgress(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s"}
	exec := &stuckExecutor{scriptedExecutor: *newScriptedExecutor([]string{""}), busy: true}
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	d := completion.NewDetector(store, factory, completion.Config{"agent": {
		Tier: completion.TierMarker, MaxTurnDuration: 600 * time.Millisecond, NoProgressTimeout: 150 * time.Millisecond,
	}}, t.TempDir(), completion.WithProgressPollInterval(20*time.Millisecond))

	if timeout := waitTimedOut(t, d, task, 3*time.Second); timeout.Reason != orchestrator.TimeoutMaxTurnDuration {
		t.Errorf("timeout = %+v, want only the max-turn limit to fire on a busy pane", timeout)
	}
}

// Cancelling the request is not a timeout.
func TestDetector_CancelIsNotTimeout(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s"}
	exec := &stuckExecutor{scriptedExecutor: *newScriptedExecutor([]string{""}), busy: true}
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	d := completion.NewDetector(store, factory, completion.Config{"agent": {Tier: completion.TierMarker}}, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := d.Wait(ctx, task)
	var timeout *orchestrator.TurnTimeoutError
	if errors.As(err, &timeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait = %v, want the caller's own context error, not a turn timeout", err)
	}
}

// LOOM-97: a prompt the agent-type's DetectPrompt recognises, showing on
// two consecutive checks, ends the wait at once with the prompt — not
// after the no-progress timeout.
func TestDetector_PromptNeedsAttention(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s"}
	exec := &stuckExecutor{scriptedExecutor: *newScriptedExecutor([]string{""})}
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	detect := func(screen string) *registry.Attention {
		if screen == "Allow this command? [y/n]" {
			return &registry.Attention{Kind: registry.AttentionPermission, Question: screen}
		}
		return nil
	}
	d := completion.NewDetector(store, factory, completion.Config{"agent": {
		Tier: completion.TierMarker, MaxTurnDuration: time.Hour, NoProgressTimeout: time.Hour, DetectPrompt: detect,
	}}, t.TempDir(), completion.WithProgressPollInterval(20*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := d.Wait(ctx, task)
	var attention *orchestrator.NeedsAttentionError
	if !errors.As(err, &attention) {
		t.Fatalf("Wait = %v, want *orchestrator.NeedsAttentionError", err)
	}
	if attention.Attention.Kind != registry.AttentionPermission || attention.Attention.Question != "Allow this command? [y/n]" {
		t.Errorf("attention = %+v", attention.Attention)
	}
}

// A busy pane that never shows a prompt is never reported as one.
func TestDetector_NoPromptOnBusyPane(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s"}
	exec := &stuckExecutor{scriptedExecutor: *newScriptedExecutor([]string{""}), busy: true}
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	detect := func(string) *registry.Attention { return nil }
	d := completion.NewDetector(store, factory, completion.Config{"agent": {
		Tier: completion.TierMarker, MaxTurnDuration: 300 * time.Millisecond, DetectPrompt: detect,
	}}, t.TempDir(), completion.WithProgressPollInterval(20*time.Millisecond))
	if timeout := waitTimedOut(t, d, task, 3*time.Second); timeout.Reason != orchestrator.TimeoutMaxTurnDuration {
		t.Errorf("timeout = %+v, want the max-turn limit", timeout)
	}
}

// orchestrator.WithSettle: a turn that ends without its completion
// signal (the user denied a permission) is over once the pane settles.
func TestDetector_Settle(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s"}
	exec := &stuckExecutor{scriptedExecutor: *newScriptedExecutor([]string{""})}
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	d := completion.NewDetector(store, factory, completion.Config{"agent": {
		Tier: completion.TierMarker, MaxTurnDuration: time.Hour, NoProgressTimeout: time.Hour,
	}}, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Wait(orchestrator.WithSettle(ctx, 300*time.Millisecond), task); err != nil {
		t.Errorf("Wait = %v, want the settled pane to end the turn", err)
	}
}
