package orchestrator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// LOOM-271 (TUI S10): watch holds (docs/design/tui-client-design.md,
// "Watching"). A watch hold says who is looking at a task's pane, and
// nothing else: the task's status, its driver, its running turn and the
// tmux clients attached to it are all left alone.

// watch takes a watch hold on taskID for session.
func watch(t *testing.T, o *orchestrator.Orchestrator, taskID, session string) *registry.TaskHold {
	t.Helper()
	res, err := o.Takeover(context.Background(), orchestrator.TakeoverRequest{TaskID: taskID, SessionID: session,
		ClientKind: "tui", ClientLabel: "laptop", Mode: registry.HoldModeWatch})
	if err != nil {
		t.Fatalf("Takeover (watch): %v", err)
	}
	if res.Hold == nil || res.Hold.Mode != registry.HoldModeWatch {
		t.Fatalf("Takeover (watch) = %+v; want a watch hold, never another mode", res.Hold)
	}
	return res.Hold
}

// typed is how many times keys were typed into task's pane.
func typed(exec *fakeExecutor, task *registry.Task) int {
	s := exec.sessionFor(task.TmuxSession)
	if s == nil {
		return 0
	}
	exec.mu.Lock()
	defer exec.mu.Unlock()
	return len(s.keys)
}

func activeHolds(t *testing.T, store registry.Store) []*registry.TaskHold {
	t.Helper()
	holds, err := store.ListActiveTaskHolds(context.Background())
	if err != nil {
		t.Fatalf("ListActiveTaskHolds: %v", err)
	}
	return holds
}

// A watch hold is taken on any task with a pane to look at, and leaves
// it exactly as it was: its status, its prompt, nobody driving, nothing
// typed and nobody detached.
func TestWatch_LeavesTheTaskAsItIs(t *testing.T) {
	for name, tc := range map[string]struct {
		status registry.TaskStatus
		class  registry.ErrorClass
	}{
		"running":         {registry.TaskStatusRunning, ""},
		"awaiting-input":  {registry.TaskStatusAwaitingInput, ""},
		"needs-attention": {registry.TaskStatusNeedsAttention, ""},
		"failed, kept":    {registry.TaskStatusFailed, registry.ErrorClassLoginRequired},
	} {
		t.Run(name, func(t *testing.T) {
			store, ws, exec, o, clock := holdSetup(t)
			ctx := context.Background()
			task := taskIn(t, store, o, ws, "conv-1", tc.status, tc.class)
			keys := typed(exec, task)

			res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1",
				ClientKind: "tui", ClientLabel: "laptop", Mode: registry.HoldModeWatch})
			if err != nil {
				t.Fatalf("Takeover (watch): %v", err)
			}
			h := res.Hold
			if res.Renewed || res.Replaced != nil || h.Mode != registry.HoldModeWatch || h.TaskID != task.ID || h.SessionID != "sess-1" ||
				h.ClientKind != "tui" || h.ClientLabel != "laptop" || h.PrevStatus != tc.status || h.TTL != orchestrator.DefaultHoldTTL ||
				!h.AcquiredAt.Equal(clock.Now()) || !h.ExpiresAt.Equal(clock.Now().Add(orchestrator.DefaultHoldTTL)) || !h.Active() {
				t.Errorf("result = %+v, hold %+v", res, h)
			}
			if res.Task.Status != tc.status {
				t.Errorf("returned task = %s, want it still %s", res.Task.Status, tc.status)
			}
			stored := mustTask(t, store, task.ID)
			if stored.Status != tc.status {
				t.Fatalf("stored status = %s; a watch must leave it %s", stored.Status, tc.status)
			}
			if tc.status == registry.TaskStatusNeedsAttention && stored.Attention == nil {
				t.Error("the prompt was cleared by a watch")
			}
			if d, err := store.GetActiveDriveHold(ctx, task.ID); !errors.Is(err, registry.ErrNotFound) {
				t.Errorf("drive hold after a watch = %+v, %v; want none", d, err)
			}
			if got := exec.detachedSessions(); len(got) != 0 {
				t.Errorf("a watch detached %v", got)
			}
			if got := typed(exec, task); got != keys {
				t.Errorf("a watch typed into the pane: %d writes, was %d", got, keys)
			}
			// Automation still types into a pane that is only watched.
			if tc.status == registry.TaskStatusAwaitingInput {
				if err := o.SendMessage(ctx, task.ID, "echo hi"); err != nil {
					t.Errorf("SendMessage while watched = %v; automation must keep typing", err)
				}
			}
		})
	}
}

// A turn in flight on a watched task carries on: only a drive takeover
// ends its wait.
func TestWatch_RunningTurnCarriesOn(t *testing.T) {
	store, ws, _, detector, o := setupBlocking(t)
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusRunning, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := waitResult(ctx, o, task.ID)
	<-detector.entered

	watch(t, o, task.ID, "sess-1")
	select {
	case err := <-done:
		t.Fatalf("the turn's wait ended when its task was watched: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusRunning {
		t.Fatalf("status = %s, want running", got.Status)
	}
}

// Any number of sessions watch a task beside the one that drives it: a
// watch is never refused as held, and takes nothing from the driver.
// Watching gives no claim on the pane either: the watcher's own drive
// takeover is still refused as held.
func TestWatch_ManyBesideOneDriveHold(t *testing.T) {
	store, ws, exec, o, _ := holdSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	drive := takeOver(t, o, task.ID) // sess-1

	w2 := watch(t, o, task.ID, "sess-2")
	w3 := watch(t, o, task.ID, "sess-3")
	if w2.ID == w3.ID || w2.ID == drive.ID || w3.ID == drive.ID {
		t.Fatalf("holds = %s, %s, %s; want three different ones", drive.ID, w2.ID, w3.ID)
	}
	if w2.PrevStatus != registry.TaskStatusHumanTakeover {
		t.Errorf("watch of a driven task: prev status = %s, want human-takeover", w2.PrevStatus)
	}
	if got := len(activeHolds(t, store)); got != 3 {
		t.Errorf("active holds = %d, want the drive hold and two watch holds", got)
	}
	if d, err := store.GetActiveDriveHold(ctx, task.ID); err != nil || d.ID != drive.ID {
		t.Errorf("drive hold = %+v, %v; want sess-1's, untouched", d, err)
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("status = %s, want human-takeover", got.Status)
	}
	if got := exec.detachedSessions(); len(got) != 0 {
		t.Errorf("watching detached %v", got)
	}

	var held *orchestrator.TaskHeldError
	if _, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-2"}); !errors.As(err, &held) || held.Hold.ID != drive.ID {
		t.Errorf("a watcher's drive takeover = %v; want TaskHeldError with the driver's hold", err)
	}
	// The driver's own takeover still finds its drive hold, not a watch.
	again, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1"})
	if err != nil || !again.Renewed || again.Hold.ID != drive.ID {
		t.Errorf("the driver's takeover = %+v, %v; want its drive hold renewed", again, err)
	}
}

// A session has one watch hold per task: asking again renews and returns
// it, as for a drive hold. Its drive hold on the task is never what a
// watch request gets.
func TestWatch_SameSessionGetsItsHoldBack(t *testing.T) {
	store, ws, _, o, clock := holdSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	other := taskIn(t, store, o, ws, "conv-2", registry.TaskStatusAwaitingInput, "")

	first := watch(t, o, task.ID, "sess-1")
	clock.Advance(10 * time.Second)
	res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1", Mode: registry.HoldModeWatch,
		ClientLabel: "other", TTL: 300 * time.Second})
	if err != nil {
		t.Fatalf("second watch: %v", err)
	}
	if !res.Renewed || res.Hold.ID != first.ID || res.Hold.Mode != registry.HoldModeWatch || !res.Hold.RenewedAt.Equal(clock.Now()) ||
		res.Hold.TTL != orchestrator.DefaultHoldTTL || !res.Hold.ExpiresAt.Equal(clock.Now().Add(orchestrator.DefaultHoldTTL)) {
		t.Errorf("second watch = %+v, hold %+v; want the first hold renewed", res, res.Hold)
	}
	// Its watch of another task is another hold.
	if h := watch(t, o, other.ID, "sess-1"); h.ID == first.ID {
		t.Error("a watch of another task returned this task's hold")
	}

	// A session that drives the task and asks to watch gets a watch hold.
	drive := takeOver(t, o, task.ID) // sess-1
	if drive.Mode != registry.HoldModeDrive || drive.ID == first.ID {
		t.Fatalf("drive takeover by a watching session = %+v; want a new drive hold", drive)
	}
	if h := watch(t, o, task.ID, "sess-1"); h.ID != first.ID {
		t.Errorf("watch by the driving session = %s, want its watch hold %s", h.ID, first.ID)
	}
	if got := len(activeHolds(t, store)); got != 3 {
		t.Errorf("active holds = %d, want 3 (two watch, one drive)", got)
	}
}

// force is for taking the pane from its holder, which a watch never
// does; an ended task has no pane to look at.
func TestWatch_Refusals(t *testing.T) {
	store, ws, exec, o, _ := holdSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	takeOver(t, o, task.ID) // sess-1 drives

	var invalid *orchestrator.InvalidTakeoverError
	if _, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-2", Mode: registry.HoldModeWatch, Force: true}); !errors.As(err, &invalid) {
		t.Errorf("watch with force = %v, want InvalidTakeoverError", err)
	}
	if got := exec.detachedSessions(); len(got) != 0 {
		t.Errorf("a refused watch with force detached %v", got)
	}
	if _, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-2", Mode: registry.HoldModeWatch, TTL: time.Second}); !errors.As(err, &invalid) {
		t.Errorf("watch with a 1 s lease = %v, want InvalidTakeoverError", err)
	}
	done := taskIn(t, store, o, ws, "conv-2", registry.TaskStatusCompleted, "")
	timedOut := taskIn(t, store, o, ws, "conv-3", registry.TaskStatusFailed, registry.ErrorClassTimeout)
	for _, id := range []string{done.ID, timedOut.ID} {
		if _, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: id, SessionID: "sess-2", Mode: registry.HoldModeWatch}); !errors.Is(err, orchestrator.ErrTaskInactive) {
			t.Errorf("watch of ended task %s = %v, want ErrTaskInactive", id, err)
		}
	}
	if _, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: "no-such", SessionID: "sess-2", Mode: registry.HoldModeWatch}); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("watch of an unknown task = %v, want ErrNotFound", err)
	}
	if got := len(activeHolds(t, store)); got != 1 {
		t.Errorf("active holds = %d, want only the drive hold", got)
	}
}

// Ending a watch hold changes nothing but the hold: the task isn't
// handed back, its driver stays, nobody is detached. A Take back ends
// the drive hold and leaves the watchers' holds.
func TestWatch_ReleaseLeavesTheTask(t *testing.T) {
	store, ws, exec, o, _ := holdSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusNeedsAttention, "")
	drive := takeOver(t, o, task.ID) // sess-1
	w2 := watch(t, o, task.ID, "sess-2")
	w3 := watch(t, o, task.ID, "sess-3")

	rel, err := o.Release(ctx, task.ID, w2.ID)
	if err != nil || !rel.Ended || rel.TaskReleased || rel.Hold.EndedReason != registry.HoldEndReleased || rel.Task.Status != registry.TaskStatusHumanTakeover {
		t.Fatalf("release of a watch hold = %+v, %v; want it ended and the task left held", rel, err)
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover || got.Attention == nil {
		t.Errorf("task after a watcher let go = %s, attention %+v; want it untouched", got.Status, got.Attention)
	}
	if d, err := store.GetActiveDriveHold(ctx, task.ID); err != nil || d.ID != drive.ID {
		t.Errorf("drive hold after a watcher let go = %+v, %v", d, err)
	}
	if got := exec.detachedSessions(); len(got) != 0 {
		t.Errorf("a watcher letting go detached %v", got)
	}
	if _, err := o.Heartbeat(ctx, task.ID, w2.ID, "sess-2"); !isHoldEnded(err, registry.HoldEndReleased) {
		t.Errorf("heartbeat of a released watch hold = %v; want hold ended (released)", err)
	}
	// The other watcher's lease is its own to renew.
	if _, err := o.Heartbeat(ctx, task.ID, w3.ID, "sess-2"); !errors.Is(err, orchestrator.ErrHoldNotOwned) {
		t.Errorf("heartbeat of another session's watch hold = %v, want ErrHoldNotOwned", err)
	}
	if _, err := o.Heartbeat(ctx, task.ID, w3.ID, "sess-3"); err != nil {
		t.Errorf("heartbeat of a watch hold: %v", err)
	}

	// Take back: the driver's hold ends, the remaining watcher's doesn't.
	back, err := o.Release(ctx, task.ID, "")
	if err != nil || !back.TaskReleased || back.Hold.ID != drive.ID || back.Hold.EndedReason != registry.HoldEndTakenBack {
		t.Fatalf("take back = %+v, %v", back, err)
	}
	if h := mustHold(t, store, w3.ID); !h.Active() {
		t.Errorf("take back ended a watch hold: %+v", h)
	}
	// With only a watcher left, a Take back finds nothing to take back.
	none, err := o.Release(ctx, task.ID, "")
	if err != nil || none.Hold != nil || none.Ended || none.TaskReleased {
		t.Errorf("take back of a task that is only watched = %+v, %v; want nothing done", none, err)
	}
	if h := mustHold(t, store, w3.ID); !h.Active() {
		t.Errorf("take back of an only-watched task ended the watch hold: %+v", h)
	}
}

// A watch hold nobody renews ends as expired, and that is all: nobody is
// detached (a read-only client can't collide with automation, and the
// detach would throw out the driver too), and the task and its driver
// stay.
func TestHoldSweeper_WatchExpiryDetachesNobody(t *testing.T) {
	store, ws, exec, o, clock := sweepSetup(t)
	ctx := context.Background()
	driven := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	free := taskIn(t, store, o, ws, "conv-2", registry.TaskStatusRunning, "")
	res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: driven.ID, SessionID: "sess-1", TTL: orchestrator.MaxHoldTTL})
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	drive := res.Hold
	w1 := watch(t, o, driven.ID, "sess-2")
	w2 := watch(t, o, free.ID, "sess-2")
	sweeper := orchestrator.NewHoldSweeper(o)

	clock.Advance(30 * time.Second)
	if _, err := o.Heartbeat(ctx, free.ID, w2.ID, "sess-2"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	clock.Advance(16 * time.Second) // past w1's lease, inside w2's renewed one
	if n := sweeper.Sweep(ctx); n != 1 {
		t.Fatalf("sweep ended %d holds, want only the watch hold nobody renewed", n)
	}
	if h := mustHold(t, store, w1.ID); h.Active() || h.EndedReason != registry.HoldEndExpired {
		t.Errorf("unrenewed watch hold = %+v; want ended as expired", h)
	}
	if !mustHold(t, store, w2.ID).Active() || !mustHold(t, store, drive.ID).Active() {
		t.Error("the renewed watch hold or the drive hold was ended too")
	}
	clock.Advance(30 * time.Second) // past w2's too
	if n := sweeper.Sweep(ctx); n != 1 {
		t.Fatalf("second sweep ended %d holds, want 1", n)
	}
	if got := exec.detachedSessions(); len(got) != 0 {
		t.Errorf("a watch hold's expiry detached %v; it detaches nobody", got)
	}
	if got := mustTask(t, store, driven.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("driven task = %s; a watcher's expiry must not hand it back", got.Status)
	}
	if got := mustTask(t, store, free.ID); got.Status != registry.TaskStatusRunning {
		t.Errorf("watched task = %s, want it still running", got.Status)
	}
}

// A watch hold's end needs nothing from the target, so it ends while the
// target is down: also in the pass that gives up on that target because
// a drive hold's clients couldn't be detached.
func TestHoldSweeper_WatchEndsWithTheTargetDown(t *testing.T) {
	store, ws, exec, o, clock := sweepSetup(t)
	ctx := context.Background()
	driven := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	watched := taskIn(t, store, o, ws, "conv-2", registry.TaskStatusAwaitingInput, "")
	drive := takeOver(t, o, driven.ID) // sess-1; listed first, as the oldest
	clock.Advance(time.Second)
	w1 := watch(t, o, driven.ID, "sess-2")
	w2 := watch(t, o, watched.ID, "sess-2")
	sweeper := orchestrator.NewHoldSweeper(o)

	clock.Advance(time.Minute)
	exec.mu.Lock()
	exec.unreachable = true
	exec.mu.Unlock()
	if n := sweeper.Sweep(ctx); n != 2 {
		t.Fatalf("sweep with the target down ended %d holds, want the two watch holds", n)
	}
	for _, id := range []string{w1.ID, w2.ID} {
		if h := mustHold(t, store, id); h.Active() || h.EndedReason != registry.HoldEndExpired {
			t.Errorf("watch hold %s = %+v; want ended as expired with the target down", id, h)
		}
	}
	// The drive hold still fails closed.
	if !mustHold(t, store, drive.ID).Active() {
		t.Error("the drive hold ended though its clients couldn't be detached")
	}
	if got := mustTask(t, store, driven.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("driven task = %s; want human-takeover while detaching fails", got.Status)
	}
}

// A revoked session's watch hold ends as session_revoked without a
// detach; the driver it was watching is left in the pane.
func TestEndSessionHolds_WatchDetachesNobody(t *testing.T) {
	store, ws, exec, o, _ := sweepSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	drive := takeOver(t, o, task.ID) // sess-1
	w := watch(t, o, task.ID, "sess-2")

	n, err := o.EndSessionHolds(ctx, "sess-2")
	if err != nil || n != 1 {
		t.Fatalf("EndSessionHolds = %d, %v; want 1", n, err)
	}
	if h := mustHold(t, store, w.ID); h.Active() || h.EndedReason != registry.HoldEndSessionRevoked {
		t.Errorf("watch hold = %+v; want ended as session_revoked", h)
	}
	if got := exec.detachedSessions(); len(got) != 0 {
		t.Errorf("revoking a watcher detached %v", got)
	}
	if !mustHold(t, store, drive.ID).Active() || mustTask(t, store, task.ID).Status != registry.TaskStatusHumanTakeover {
		t.Error("revoking a watcher ended the driver's hold or handed the task back")
	}
}

// A watch hold on a task that ended ends as task_ended, as a drive hold
// does.
func TestHoldSweeper_WatchTaskEnded(t *testing.T) {
	store, ws, exec, o, _ := sweepSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusRunning, "")
	w := watch(t, o, task.ID, "sess-1")
	if err := o.Complete(ctx, task.ID, registry.TaskStatusRunning, "done"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if n := orchestrator.NewHoldSweeper(o).Sweep(ctx); n != 1 {
		t.Fatalf("sweep ended %d holds, want 1", n)
	}
	if h := mustHold(t, store, w.ID); h.Active() || h.EndedReason != registry.HoldEndTaskEnded {
		t.Errorf("watch hold = %+v; want ended as task_ended", h)
	}
	if got := exec.detachedSessions(); len(got) != 0 {
		t.Errorf("detached %v", got)
	}
}
