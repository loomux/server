package providers_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
)

// LOOM-177 (gap G4): a persistent created machine nobody uses is stopped
// once idle for IdleStop, and the next dispatch starts it again.

// stubActivity is an Activity the test sets.
type stubActivity struct {
	mu   sync.Mutex
	busy bool
	last time.Time
}

func (a *stubActivity) get(context.Context, string) (bool, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busy, a.last, nil
}

func (a *stubActivity) set(busy bool, last time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.busy, a.last = busy, last
}

// idleClock is the Manager's clock, moved by the test.
type idleClock struct{ ns atomic.Int64 }

func (c *idleClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *idleClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// idleMachine is a harness with IdleStop an hour and a running machine.
func idleMachine(t *testing.T, persistent bool) (*harness, *registry.Target, *idleClock, *stubActivity) {
	t.Helper()
	h := newHarness(t, nil)
	clock := &idleClock{}
	clock.ns.Store(time.Now().UnixNano())
	act := &stubActivity{}
	h.m.Now = clock.now
	h.m.IdleStop, h.m.Activity = time.Hour, act.get
	target, env := h.create(t, "idle", persistent)
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)
	// The create's job is still registered a moment after the row says
	// running, and StopIdle passes over a machine a job follows: wait it
	// out, or a sweep right after skips the machine (LOOM-217's flake).
	deadline := time.Now().Add(10 * time.Second)
	for h.m.FollowingForTest(env.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the create's job still follows the machine 10 s after it ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return h, target, clock, act
}

// fakeMachine is the plugin's own view of the machine.
func (h *harness) fakeMachine(t *testing.T, envID string) protocol.Environment {
	t.Helper()
	var pe protocol.Environment
	if err := h.pm.Call(context.Background(), h.pluginID, protocol.MethodTargetsGet, protocol.IDParams{ID: envID}, &pe); err != nil {
		t.Fatal(err)
	}
	return pe
}

func (h *harness) status(t *testing.T, targetID string) *registry.Environment {
	t.Helper()
	env, err := h.m.Get(context.Background(), targetID)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestIdleMachineStoppedAndStartedAgain(t *testing.T) {
	h, target, clock, _ := idleMachine(t, true)
	ctx := context.Background()
	h.m.StopIdle(ctx)
	clock.advance(59 * time.Minute)
	h.m.StopIdle(ctx)
	if env := h.status(t, target.ID); env.Status != registry.EnvironmentRunning {
		t.Fatalf("59 minutes idle: %s, want running", env.Status)
	}
	clock.advance(2 * time.Minute)
	h.m.StopIdle(ctx)
	env := h.status(t, target.ID)
	if env.Status != registry.EnvironmentStopped || !strings.Contains(env.StatusReason, "stopped after 1h0m0s idle") {
		t.Fatalf("61 minutes idle: %s (%s), want stopped, saying why", env.Status, env.StatusReason)
	}
	if m := h.fakeMachine(t, env.ID); m.Status != "stopped" {
		t.Errorf("the plugin's machine is %s, want stopped", m.Status)
	}
	// The next dispatch starts it again, as any stopped machine.
	if err := h.m.EnsureRunning(ctx, target.ID); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)
}

func TestIdleStopSparesTheBusy(t *testing.T) {
	t.Run("a task unfinished", func(t *testing.T) {
		h, target, clock, act := idleMachine(t, true)
		act.set(true, clock.now().Add(-3*time.Hour))
		clock.advance(2 * time.Hour)
		h.m.StopIdle(context.Background())
		if env := h.status(t, target.ID); env.Status != registry.EnvironmentRunning {
			t.Errorf("busy 2 hours on: %s, want running", env.Status)
		}
	})
	t.Run("a task changed recently", func(t *testing.T) {
		h, target, clock, act := idleMachine(t, true)
		clock.advance(2 * time.Hour)
		act.set(false, clock.now().Add(-30*time.Minute))
		h.m.StopIdle(context.Background())
		if env := h.status(t, target.ID); env.Status != registry.EnvironmentRunning {
			t.Fatalf("a task 30 minutes ago: %s, want running", env.Status)
		}
		clock.advance(31 * time.Minute)
		h.m.StopIdle(context.Background())
		if env := h.status(t, target.ID); env.Status != registry.EnvironmentStopped {
			t.Errorf("a task 61 minutes ago: %s, want stopped", env.Status)
		}
	})
	t.Run("handed to a dispatch", func(t *testing.T) {
		h, target, clock, _ := idleMachine(t, true)
		clock.advance(59 * time.Minute)
		if err := h.m.EnsureRunning(context.Background(), target.ID); err != nil {
			t.Fatal(err)
		}
		clock.advance(2 * time.Minute)
		h.m.StopIdle(context.Background())
		if env := h.status(t, target.ID); env.Status != registry.EnvironmentRunning {
			t.Errorf("a dispatch took it 2 minutes ago: %s, want running", env.Status)
		}
	})
	t.Run("ephemeral", func(t *testing.T) {
		h, target, clock, _ := idleMachine(t, false)
		clock.advance(2 * time.Hour)
		h.m.StopIdle(context.Background())
		if env := h.status(t, target.ID); env.Status != registry.EnvironmentRunning {
			t.Errorf("an ephemeral machine 2 hours idle: %s, want running (its stop would destroy it)", env.Status)
		}
	})
	t.Run("off", func(t *testing.T) {
		h, target, clock, _ := idleMachine(t, true)
		h.m.IdleStop = 0
		clock.advance(48 * time.Hour)
		h.m.StopIdle(context.Background())
		if env := h.status(t, target.ID); env.Status != registry.EnvironmentRunning {
			t.Errorf("IdleStop off: %s, want running", env.Status)
		}
	})
}

// A restarted host counts a running machine's idleness from its own
// start: it never stops one early for want of having seen it start.
func TestIdleStopAfterARestart(t *testing.T) {
	h, target, clock, _ := idleMachine(t, true)
	m := providers.NewManager(h.store, h.pm, "inst", nil, nil)
	defer m.Close()
	m.Now, m.IdleStop, m.Activity = clock.now, time.Hour, (&stubActivity{}).get
	clock.advance(3 * time.Hour) // long idle, unseen by the new manager
	m.StopIdle(context.Background())
	if env := h.status(t, target.ID); env.Status != registry.EnvironmentRunning {
		t.Fatalf("just after a restart: %s, want running", env.Status)
	}
	clock.advance(61 * time.Minute)
	m.StopIdle(context.Background())
	if env := h.status(t, target.ID); env.Status != registry.EnvironmentStopped {
		t.Errorf("an hour after the restart: %s, want stopped", env.Status)
	}
}

// TaskActivity reads the target's tasks: busy while one is unfinished,
// and the latest change otherwise.
func TestTaskActivity(t *testing.T) {
	h := newHarness(t, nil)
	target, _ := h.create(t, "tasks", true)
	ctx := context.Background()
	act := providers.TaskActivity(h.store)
	if busy, last, err := act(ctx, target.ID); err != nil || busy || !last.IsZero() {
		t.Fatalf("no workspaces: %v %v %v", busy, last, err)
	}
	ws := &registry.Workspace{ID: "ws-idle", Name: "ws-idle", Path: "/data/work/x", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	done := &registry.Task{ID: "t-done", WorkspaceID: ws.ID, Kind: registry.TaskKindShell, TmuxSession: "s1", ConversationID: "c", Status: registry.TaskStatusCompleted}
	if err := h.store.CreateTask(ctx, done); err != nil {
		t.Fatal(err)
	}
	busy, last, err := act(ctx, target.ID)
	if err != nil || busy || last.IsZero() {
		t.Fatalf("a finished task: busy %v, last %v, %v; want idle since it", busy, last, err)
	}
	for _, status := range []registry.TaskStatus{registry.TaskStatusRunning, registry.TaskStatusAwaitingInput, registry.TaskStatusHumanTakeover, registry.TaskStatusNeedsAttention} {
		open := &registry.Task{ID: "t-" + string(status), WorkspaceID: ws.ID, Kind: registry.TaskKindShell, TmuxSession: "s-" + string(status), ConversationID: "c", Status: status}
		if err := h.store.CreateTask(ctx, open); err != nil {
			t.Fatal(err)
		}
		if busy, _, err := act(ctx, target.ID); err != nil || !busy {
			t.Errorf("a task %s: busy %v, %v; want busy", status, busy, err)
		}
		open.Status = registry.TaskStatusFailed
		if err := h.store.UpdateTask(ctx, open); err != nil {
			t.Fatal(err)
		}
	}
	if busy, _, _ := act(ctx, "another-target"); busy {
		t.Error("another target's tasks counted")
	}
}
