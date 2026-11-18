package orchestrator_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// LOOM-265: the expiry sweeper, restart grace, revocation and detaching a
// hold's clients (docs/design/tui-client-design.md, "Expiry, crashes and
// restarts").

// sweepSetup is holdSetup on a clock that starts at the real time, since
// a session's CreatedAt is stamped by the store's own clock, with
// sessions sess-1 and sess-2 in the store.
func sweepSetup(t *testing.T) (registry.Store, *registry.Workspace, *fakeExecutor, *orchestrator.Orchestrator, *fakeClock) {
	t.Helper()
	store, ws, exec, o, clock := holdSetup(t)
	clock.mu.Lock()
	clock.now = time.Now().UTC()
	clock.mu.Unlock()
	for _, id := range []string{"sess-1", "sess-2"} {
		if err := store.CreateSession(context.Background(), &registry.Session{ID: id, TokenHash: id + "-hash", LastUsedAt: clock.Now()}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	return store, ws, exec, o, clock
}

func mustHold(t *testing.T, store registry.Store, id string) *registry.TaskHold {
	t.Helper()
	h, err := store.GetTaskHold(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTaskHold: %v", err)
	}
	return h
}

// detached returns the sessions DetachClients was called for, in order.
func (e *fakeExecutor) detachedSessions() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.detached...)
}

// A drive hold nobody renews ends as expired once its lease runs out:
// the session's clients are detached and the task is handed back.
// Before that, and after a heartbeat, it is left alone.
func TestHoldSweeper_Expiry(t *testing.T) {
	store, ws, exec, o, clock := sweepSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusNeedsAttention, "")
	hold := takeOver(t, o, task.ID)
	sweeper := orchestrator.NewHoldSweeper(o)

	clock.Advance(30 * time.Second)
	if n := sweeper.Sweep(ctx); n != 0 {
		t.Fatalf("sweep inside the lease ended %d holds", n)
	}
	if _, err := o.Heartbeat(ctx, task.ID, hold.ID, "sess-1"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	clock.Advance(30 * time.Second) // 60 s after taking, 30 s after the heartbeat
	if n := sweeper.Sweep(ctx); n != 0 || !mustHold(t, store, hold.ID).Active() {
		t.Fatalf("sweep inside a renewed lease ended %d holds", n)
	}
	if got := exec.detachedSessions(); len(got) != 0 {
		t.Fatalf("detached %v while the lease held", got)
	}

	clock.Advance(16 * time.Second) // past the renewed lease
	if n := sweeper.Sweep(ctx); n != 1 {
		t.Fatalf("sweep past the lease ended %d holds, want 1", n)
	}
	h := mustHold(t, store, hold.ID)
	if h.Active() || h.EndedReason != registry.HoldEndExpired || !h.EndedAt.Equal(clock.Now()) {
		t.Errorf("hold = %+v; want ended as expired now", h)
	}
	got := mustTask(t, store, task.ID)
	if got.Status != registry.TaskStatusAwaitingInput || got.Attention != nil {
		t.Errorf("task = %s, attention %+v; want awaiting-input, prompt cleared", got.Status, got.Attention)
	}
	if d := exec.detachedSessions(); len(d) != 1 || d[0] != task.TmuxSession {
		t.Errorf("detached %v, want [%s]", d, task.TmuxSession)
	}
	if _, err := o.Heartbeat(ctx, task.ID, hold.ID, "sess-1"); !isHoldEnded(err, registry.HoldEndExpired) {
		t.Errorf("heartbeat after expiry = %v; want hold ended (expired)", err)
	}
	if n := sweeper.Sweep(ctx); n != 0 {
		t.Errorf("second sweep ended %d holds", n)
	}
	if d := exec.detachedSessions(); len(d) != 1 {
		t.Errorf("second sweep detached again: %v", d)
	}
}

func isHoldEnded(err error, reason registry.HoldEndReason) bool {
	var ended *orchestrator.HoldEndedError
	return errors.As(err, &ended) && ended.Hold.EndedReason == reason
}

// Fails closed: while the clients can't be detached the hold stays and
// the task stays human-takeover, so automation never types into a pane
// someone may still be in. The next sweep tries again.
func TestHoldSweeper_DetachFailureKeepsTheHold(t *testing.T) {
	store, ws, exec, o, clock := sweepSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	hold := takeOver(t, o, task.ID)
	sweeper := orchestrator.NewHoldSweeper(o)

	clock.Advance(time.Minute)
	exec.mu.Lock()
	exec.unreachable = true
	exec.mu.Unlock()
	if n := sweeper.Sweep(ctx); n != 0 {
		t.Fatalf("sweep with the target down ended %d holds", n)
	}
	if !mustHold(t, store, hold.ID).Active() {
		t.Fatal("hold ended though its clients couldn't be detached")
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Fatalf("status = %s; want human-takeover while detaching fails", got.Status)
	}

	exec.mu.Lock()
	exec.unreachable = false
	exec.mu.Unlock()
	if n := sweeper.Sweep(ctx); n != 1 {
		t.Fatalf("sweep with the target back ended %d holds, want 1", n)
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("status = %s; want awaiting-input", got.Status)
	}
}

// A hold on a task whose target is local, on a server with local
// targets off (LOOM-141), fails closed: its session may still run, with
// someone in it, from before the setting changed. The hold and the
// task's human-takeover stay.
func TestHoldSweeper_LocalTargetsOffFailsClosed(t *testing.T) {
	store, ws, exec, o, clock := sweepSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	hold := takeOver(t, o, task.ID)
	off := orchestrator.New(store, func(*registry.Target) (targets.TargetExecutor, error) {
		return nil, targets.ErrLocalTargetsOff
	}, &hookDetector{}, orchestrator.WithClock(clock.Now))
	clock.Advance(time.Minute)
	if n := orchestrator.NewHoldSweeper(off).Sweep(ctx); n != 0 {
		t.Fatalf("ended %d holds, want none", n)
	}
	if !mustHold(t, store, hold.ID).Active() {
		t.Error("hold ended without its clients detached")
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("status = %s", got.Status)
	}
	if _, err := off.Release(ctx, task.ID, ""); !errors.Is(err, orchestrator.ErrDetachFailed) {
		t.Errorf("take back = %v; want ErrDetachFailed", err)
	}
	if d := exec.detachedSessions(); len(d) != 0 {
		t.Errorf("detached %v", d)
	}
}

// Restart grace: a hold that expired while the server was down gets a
// full lease from the start, so a deploy longer than the TTL doesn't
// kick everyone out; one that doesn't heartbeat after it still expires.
func TestHoldSweeper_RestartGrace(t *testing.T) {
	store, ws, exec, o, clock := sweepSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	hold := takeOver(t, o, task.ID)
	long := taskIn(t, store, o, ws, "conv-2", registry.TaskStatusAwaitingInput, "")
	longHold, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: long.ID, SessionID: "sess-2", TTL: 300 * time.Second})
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}

	clock.Advance(10 * time.Minute) // the server was down
	sweeper := orchestrator.NewHoldSweeper(o)
	n, err := sweeper.Grace(ctx)
	if err != nil || n != 2 {
		t.Fatalf("Grace = %d, %v; want both holds extended", n, err)
	}
	if h := mustHold(t, store, hold.ID); !h.ExpiresAt.Equal(clock.Now().Add(45 * time.Second)) {
		t.Errorf("expires_at = %v, want start + its 45 s lease (%v)", h.ExpiresAt, clock.Now().Add(45*time.Second))
	}
	if h := mustHold(t, store, longHold.Hold.ID); !h.ExpiresAt.Equal(clock.Now().Add(300 * time.Second)) {
		t.Errorf("expires_at = %v, want start + its own 300 s lease", h.ExpiresAt)
	}
	if n := sweeper.Sweep(ctx); n != 0 {
		t.Fatalf("sweep right after the grace ended %d holds", n)
	}

	clock.Advance(46 * time.Second)
	if n := sweeper.Sweep(ctx); n != 1 {
		t.Fatalf("sweep after the grace ran out ended %d holds, want 1", n)
	}
	if h := mustHold(t, store, hold.ID); h.EndedReason != registry.HoldEndExpired {
		t.Errorf("hold = %+v", h)
	}
	if !mustHold(t, store, longHold.Hold.ID).Active() {
		t.Error("the 300 s hold ended inside its grace")
	}
	if d := exec.detachedSessions(); len(d) != 1 || d[0] != task.TmuxSession {
		t.Errorf("detached %v", d)
	}
}

// Run applies the grace before its first sweep: a hold left expired by
// the downtime survives the first ticks.
func TestHoldSweeper_RunGracesFirst(t *testing.T) {
	store, ws, _, o, clock := sweepSetup(t)
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	hold := takeOver(t, o, task.ID)
	clock.Advance(10 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		orchestrator.NewHoldSweeper(o).Run(ctx, 5*time.Millisecond)
	}()
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	h := mustHold(t, store, hold.ID)
	if !h.Active() || !h.ExpiresAt.Equal(clock.Now().Add(45*time.Second)) {
		t.Errorf("hold = %+v; want active with a lease from the start", h)
	}
}

// A hold whose session is gone (logged out, revoked, deleted) or has
// expired ends as session_revoked, detached and released, without
// waiting for its lease.
func TestHoldSweeper_SessionRevoked(t *testing.T) {
	cases := []struct {
		name string
		// revoke makes sess-1 not a live session any more.
		revoke func(t *testing.T, store registry.Store, clock *fakeClock)
		opts   []orchestrator.HoldSweeperOption
		// otherEnds: sess-2 was created with sess-1, so a max age ends
		// both.
		otherEnds bool
	}{
		{name: "deleted", revoke: func(t *testing.T, store registry.Store, _ *fakeClock) {
			if err := store.DeleteSession(context.Background(), "sess-1"); err != nil {
				t.Fatalf("DeleteSession: %v", err)
			}
		}},
		{name: "idle past its ttl", revoke: func(t *testing.T, _ registry.Store, clock *fakeClock) {
			clock.Advance(31 * time.Second) // the hold's lease (45 s) still runs
		}, opts: []orchestrator.HoldSweeperOption{orchestrator.WithHoldSessionLifetime(30*time.Second, 0)}},
		{name: "past its max age", revoke: func(t *testing.T, store registry.Store, clock *fakeClock) {
			clock.Advance(40 * time.Second)
			if err := store.TouchSession(context.Background(), "sess-1", clock.Now()); err != nil {
				t.Fatalf("TouchSession: %v", err)
			}
		}, opts: []orchestrator.HoldSweeperOption{orchestrator.WithHoldSessionLifetime(time.Hour, 30*time.Second)}, otherEnds: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, ws, exec, o, clock := sweepSetup(t)
			ctx := context.Background()
			task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
			hold := takeOver(t, o, task.ID)
			other := taskIn(t, store, o, ws, "conv-2", registry.TaskStatusAwaitingInput, "")
			otherHold, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: other.ID, SessionID: "sess-2"})
			if err != nil {
				t.Fatalf("Takeover: %v", err)
			}
			sweeper := orchestrator.NewHoldSweeper(o, tc.opts...)
			if n := sweeper.Sweep(ctx); n != 0 {
				t.Fatalf("sweep with every session live ended %d", n)
			}
			tc.revoke(t, store, clock)
			// sess-2 stays live through the same time.
			if err := store.TouchSession(ctx, "sess-2", clock.Now()); err != nil {
				t.Fatalf("TouchSession: %v", err)
			}
			if _, err := o.Heartbeat(ctx, other.ID, otherHold.Hold.ID, "sess-2"); err != nil {
				t.Fatalf("Heartbeat: %v", err)
			}
			want := 1
			if tc.otherEnds {
				want = 2
			}
			if n := sweeper.Sweep(ctx); n != want {
				t.Fatalf("sweep ended %d holds, want %d", n, want)
			}
			if h := mustHold(t, store, hold.ID); h.EndedReason != registry.HoldEndSessionRevoked {
				t.Errorf("hold = %+v; want session_revoked", h)
			}
			if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusAwaitingInput {
				t.Errorf("status = %s", got.Status)
			}
			if oh := mustHold(t, store, otherHold.Hold.ID); oh.Active() == tc.otherEnds {
				t.Errorf("the other session's hold = %+v; want ended: %v", oh, tc.otherEnds)
			}
			if d := exec.detachedSessions(); len(d) != want || d[0] != task.TmuxSession && d[len(d)-1] != task.TmuxSession {
				t.Errorf("detached %v, want %d including %s", d, want, task.TmuxSession)
			}
		})
	}
}

// A task that ended while held ends its hold as task_ended. Nothing is
// detached (nothing automated drives an ended task) and the status
// stays. A failed task whose pane was kept for a person keeps its hold.
func TestHoldSweeper_TaskEnded(t *testing.T) {
	store, ws, exec, o, _ := sweepSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	hold := takeOver(t, o, task.ID)
	login := taskIn(t, store, o, ws, "conv-2", registry.TaskStatusFailed, registry.ErrorClassLoginRequired)
	loginHold := takeOver(t, o, login.ID)

	cur := mustTask(t, store, task.ID)
	now := time.Now().UTC()
	cur.Status, cur.CompletedAt, cur.ErrorClass = registry.TaskStatusFailed, &now, registry.ErrorClassInternal
	if err := store.UpdateTask(ctx, cur); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if n := orchestrator.NewHoldSweeper(o).Sweep(ctx); n != 1 {
		t.Fatalf("ended %d holds, want 1", n)
	}
	if h := mustHold(t, store, hold.ID); h.EndedReason != registry.HoldEndTaskEnded {
		t.Errorf("hold = %+v; want task_ended", h)
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusFailed {
		t.Errorf("status = %s; want it left failed", got.Status)
	}
	if !mustHold(t, store, loginHold.ID).Active() {
		t.Error("the hold on a failed task kept for sign-in ended")
	}
	if d := exec.detachedSessions(); len(d) != 0 {
		t.Errorf("detached %v", d)
	}
}

// Revocation (logout, a revoke by id): every hold of the session ends as
// session_revoked, detached and released, and no other session's.
func TestEndSessionHolds(t *testing.T) {
	store, ws, exec, o, _ := sweepSetup(t)
	ctx := context.Background()
	a := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	b := taskIn(t, store, o, ws, "conv-2", registry.TaskStatusRunning, "")
	c := taskIn(t, store, o, ws, "conv-3", registry.TaskStatusAwaitingInput, "")
	ha, hb := takeOver(t, o, a.ID), takeOver(t, o, b.ID)
	hc, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: c.ID, SessionID: "sess-2"})
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}

	n, err := o.EndSessionHolds(ctx, "sess-1")
	if err != nil || n != 2 {
		t.Fatalf("EndSessionHolds = %d, %v; want 2", n, err)
	}
	for _, id := range []string{ha.ID, hb.ID} {
		if h := mustHold(t, store, id); h.EndedReason != registry.HoldEndSessionRevoked {
			t.Errorf("hold %s = %+v", id, h)
		}
	}
	for _, id := range []string{a.ID, b.ID} {
		if got := mustTask(t, store, id); got.Status != registry.TaskStatusAwaitingInput {
			t.Errorf("task %s = %s; want awaiting-input", id, got.Status)
		}
	}
	if !mustHold(t, store, hc.Hold.ID).Active() {
		t.Error("another session's hold ended")
	}
	if d := exec.detachedSessions(); len(d) != 2 {
		t.Errorf("detached %v; want both of sess-1's tasks", d)
	}
	if n, err := o.EndSessionHolds(ctx, "sess-1"); err != nil || n != 0 {
		t.Errorf("second EndSessionHolds = %d, %v", n, err)
	}

	// A detach that fails leaves that hold for the sweeper, whose next
	// pass finds its session gone.
	exec.mu.Lock()
	exec.unreachable = true
	exec.mu.Unlock()
	if n, err := o.EndSessionHolds(ctx, "sess-2"); err == nil || n != 0 {
		t.Errorf("EndSessionHolds with the target down = %d, %v; want an error, nothing ended", n, err)
	}
	if !mustHold(t, store, hc.Hold.ID).Active() {
		t.Error("hold ended though its clients couldn't be detached")
	}
	if err := store.DeleteSession(ctx, "sess-2"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	exec.mu.Lock()
	exec.unreachable = false
	exec.mu.Unlock()
	if n := orchestrator.NewHoldSweeper(o).Sweep(ctx); n != 1 {
		t.Fatalf("sweep ended %d, want the revoked session's hold", n)
	}
	if h := mustHold(t, store, hc.Hold.ID); h.EndedReason != registry.HoldEndSessionRevoked {
		t.Errorf("hold = %+v", h)
	}
}

// A forced takeover detaches the old holder's clients before it takes
// the task and answers, so the new holder (who attaches after the
// answer) isn't detached, and fails closed: if they can't be detached
// nothing is taken. A renewal by the holder itself detaches nobody.
func TestTakeover_ForceDetachesTheOldHolder(t *testing.T) {
	store, ws, exec, o, _ := sweepSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	takeOver(t, o, task.ID)
	takeOver(t, o, task.ID) // renewed
	if d := exec.detachedSessions(); len(d) != 0 {
		t.Fatalf("a renewal detached %v", d)
	}
	res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-2", Force: true})
	if err != nil || res.Replaced == nil {
		t.Fatalf("forced Takeover = %+v, %v", res, err)
	}
	if d := exec.detachedSessions(); len(d) != 1 || d[0] != task.TmuxSession {
		t.Errorf("detached %v, want [%s]", d, task.TmuxSession)
	}

	exec.mu.Lock()
	exec.unreachable = true
	exec.mu.Unlock()
	if _, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1", Force: true}); !errors.Is(err, orchestrator.ErrDetachFailed) {
		t.Fatalf("forced Takeover with the target down = %v; want ErrDetachFailed", err)
	}
	if h := mustHold(t, store, res.Hold.ID); !h.Active() {
		t.Errorf("the holder's hold = %+v; want it kept", h)
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("status = %s", got.Status)
	}
}

// Take back detaches the holder's clients before the task is handed
// back, and fails closed: if they can't be detached, the hold and the
// task's human-takeover stay. A holder's own release detaches nobody (its
// client has already let go).
func TestRelease_TakeBackDetaches(t *testing.T) {
	store, ws, exec, o, _ := sweepSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	hold := takeOver(t, o, task.ID)

	exec.mu.Lock()
	exec.unreachable = true
	exec.mu.Unlock()
	if _, err := o.Release(ctx, task.ID, ""); err == nil || !errors.Is(err, targets.ErrUnreachable) {
		t.Fatalf("take back with the target down = %v; want the detach's error", err)
	}
	if !mustHold(t, store, hold.ID).Active() {
		t.Fatal("hold ended though its clients couldn't be detached")
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Fatalf("status = %s", got.Status)
	}

	exec.mu.Lock()
	exec.unreachable = false
	exec.mu.Unlock()
	rel, err := o.Release(ctx, task.ID, "")
	if err != nil || !rel.Ended || !rel.TaskReleased || rel.Hold.ID != hold.ID || rel.Hold.EndedReason != registry.HoldEndTakenBack {
		t.Fatalf("take back = %+v, %v", rel, err)
	}
	if d := exec.detachedSessions(); len(d) != 1 || d[0] != task.TmuxSession {
		t.Errorf("detached %v", d)
	}

	again := takeOver(t, o, task.ID)
	if _, err := o.Release(ctx, task.ID, again.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if d := exec.detachedSessions(); len(d) != 1 {
		t.Errorf("the holder's own release detached: %v", d)
	}
}

// The sweeper racing heartbeats, takeovers and releases never leaves a
// task held by nobody in human-takeover, or handed back while a drive
// hold is active.
func TestHoldSweeper_Races(t *testing.T) {
	store, ws, _, o, clock := sweepSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	sweeper := orchestrator.NewHoldSweeper(o)

	for round := range 20 {
		hold := takeOver(t, o, task.ID)
		clock.Advance(46 * time.Second)
		var wg sync.WaitGroup
		wg.Add(4)
		go func() { defer wg.Done(); sweeper.Sweep(ctx) }()
		go func() { defer wg.Done(); _, _ = o.Heartbeat(ctx, task.ID, hold.ID, "sess-1") }()
		go func() {
			defer wg.Done()
			_, _ = o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-2", Force: round%2 == 0})
		}()
		go func() { defer wg.Done(); _, _ = o.Release(ctx, task.ID, hold.ID) }()
		wg.Wait()

		got := mustTask(t, store, task.ID)
		active, err := store.GetActiveDriveHold(ctx, task.ID)
		switch {
		case err == nil && got.Status != registry.TaskStatusHumanTakeover:
			t.Fatalf("round %d: hold %+v active on a task in %s", round, active, got.Status)
		case errors.Is(err, registry.ErrNotFound) && got.Status == registry.TaskStatusHumanTakeover:
			t.Fatalf("round %d: task in human-takeover with no hold", round)
		case err != nil && !errors.Is(err, registry.ErrNotFound):
			t.Fatalf("GetActiveDriveHold: %v", err)
		}
		// Clear for the next round.
		if _, err := o.Release(ctx, task.ID, ""); err != nil {
			t.Fatalf("Release: %v", err)
		}
	}
}

// LOOM-265 review: the sweeper's judgement, its detach and the hold's end
// are one step. A heartbeat or a forced takeover arriving during the
// detach waits for it: the heartbeat then finds the hold ended (it can't
// renew a lease already judged run out), and the takeover finds no hold
// left, takes the task fresh, and its holder isn't detached.
func TestHoldSweeper_DetachIsSerialized(t *testing.T) {
	store, ws, exec, o, clock := sweepSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	hold := takeOver(t, o, task.ID)
	clock.Advance(time.Minute)

	heartbeat := make(chan error, 1)
	taken := make(chan *orchestrator.TakeoverResult, 1)
	var once sync.Once
	exec.mu.Lock()
	exec.onDetach = func() {
		once.Do(func() {
			go func() {
				_, err := o.Heartbeat(ctx, task.ID, hold.ID, "sess-1")
				heartbeat <- err
			}()
			go func() {
				res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-2", Force: true})
				if err != nil {
					t.Errorf("Takeover: %v", err)
				}
				taken <- res
			}()
			// Give both time to reach the lock.
			time.Sleep(50 * time.Millisecond)
			select {
			case err := <-heartbeat:
				t.Errorf("a heartbeat finished during the sweeper's detach: %v", err)
				heartbeat <- err // for the check below
			default:
			}
		})
	}
	exec.mu.Unlock()

	if n := orchestrator.NewHoldSweeper(o).Sweep(ctx); n != 1 {
		t.Fatalf("ended %d holds, want 1", n)
	}
	if err := <-heartbeat; !isHoldEnded(err, registry.HoldEndExpired) {
		t.Errorf("heartbeat during the sweep = %v; want the hold ended as expired", err)
	}
	res := <-taken
	if res == nil || res.Replaced != nil || res.Hold.SessionID != "sess-2" {
		t.Fatalf("takeover during the sweep = %+v; want a fresh hold, nothing replaced", res)
	}
	if !mustHold(t, store, res.Hold.ID).Active() || mustTask(t, store, task.ID).Status != registry.TaskStatusHumanTakeover {
		t.Error("the new holder's hold or the task's human-takeover didn't survive the sweep")
	}
	if d := exec.detachedSessions(); len(d) != 1 {
		t.Errorf("detached %v; want only the sweeper's, before the new hold", d)
	}
}

// LOOM-265 review: Take back that finds no hold can't end one a takeover
// makes meanwhile without detaching it: the takeover waits, and its hold
// stands.
func TestRelease_TakeBackRacingATakeover(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	exec := newFakeExecutor()
	ctx := context.Background()
	wrapped := &firstHoldLookupStore{Store: store}
	o := orchestrator.New(wrapped, exec.factory(), &hookDetector{})
	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatal(err)
	}
	taken := make(chan *orchestrator.TakeoverResult, 1)
	wrapped.meanwhile = func() {
		go func() {
			res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-2"})
			if err != nil {
				t.Errorf("Takeover: %v", err)
			}
			taken <- res
		}()
		time.Sleep(50 * time.Millisecond)
	}
	rel, err := o.Release(ctx, task.ID, "")
	if err != nil || rel.Hold != nil || rel.Ended {
		t.Fatalf("take back with nothing held = %+v, %v", rel, err)
	}
	res := <-taken
	if res == nil {
		t.Fatal("no takeover")
	}
	if h := mustHold(t, store, res.Hold.ID); !h.Active() {
		t.Errorf("the takeover's hold = %+v; want it standing", h)
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("status = %s", got.Status)
	}
}

// firstHoldLookupStore runs meanwhile after the first GetActiveDriveHold
// only. Unlike sync.Once, later lookups (the racing takeover's) don't
// wait for it.
type firstHoldLookupStore struct {
	registry.Store
	used      atomic.Bool
	meanwhile func()
}

func (s *firstHoldLookupStore) GetActiveDriveHold(ctx context.Context, taskID string) (*registry.TaskHold, error) {
	h, err := s.Store.GetActiveDriveHold(ctx, taskID)
	if s.used.CompareAndSwap(false, true) {
		s.meanwhile()
	}
	return h, err
}

// failingGraceStore fails ExtendActiveTaskHolds while fail is set.
type failingGraceStore struct {
	registry.Store
	mu   sync.Mutex
	fail bool
}

func (s *failingGraceStore) ExtendActiveTaskHolds(ctx context.Context, from time.Time) (int, error) {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return 0, errors.New("disk on fire")
	}
	return s.Store.ExtendActiveTaskHolds(ctx, from)
}

// LOOM-265 review: while the restart grace fails Run sweeps nothing, so
// the holds the downtime left expired aren't ended; once it succeeds
// they get their lease and sweeping starts.
func TestHoldSweeper_NoSweepWithoutGrace(t *testing.T) {
	store, ws, exec, _, clock := sweepSetup(t)
	wrapped := &failingGraceStore{Store: store, fail: true}
	o := orchestrator.New(wrapped, exec.factory(), &hookDetector{}, orchestrator.WithClock(clock.Now))
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	hold := takeOver(t, o, task.ID)
	clock.Advance(10 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		orchestrator.NewHoldSweeper(o).Run(ctx, 5*time.Millisecond)
	}()
	time.Sleep(60 * time.Millisecond)
	if !mustHold(t, store, hold.ID).Active() {
		t.Fatal("a hold was swept while the grace failed")
	}
	wrapped.mu.Lock()
	wrapped.fail = false
	wrapped.mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	if h := mustHold(t, store, hold.ID); !h.Active() || !h.ExpiresAt.Equal(clock.Now().Add(45*time.Second)) {
		t.Errorf("hold = %+v; want active with a lease from the grace", h)
	}
}

// staleSessionsStore lists every session as last used an hour before it
// was, as a pass's list would show one a request renewed since.
type staleSessionsStore struct{ registry.Store }

func (s *staleSessionsStore) ListSessions(ctx context.Context) ([]*registry.Session, error) {
	list, err := s.Store.ListSessions(ctx)
	for _, sess := range list {
		sess.LastUsedAt = sess.LastUsedAt.Add(-time.Hour)
	}
	return list, err
}

// LOOM-265 review: a session the pass's list shows expired is read again
// before its holds are revoked: one renewed since keeps them.
func TestHoldSweeper_SessionRenewedSinceTheList(t *testing.T) {
	store, ws, exec, _, clock := sweepSetup(t)
	o := orchestrator.New(&staleSessionsStore{Store: store}, exec.factory(), &hookDetector{}, orchestrator.WithClock(clock.Now))
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	hold := takeOver(t, o, task.ID)
	sweeper := orchestrator.NewHoldSweeper(o, orchestrator.WithHoldSessionLifetime(30*time.Minute, 0))
	if n := sweeper.Sweep(context.Background()); n != 0 {
		t.Fatalf("ended %d holds; the session is live", n)
	}
	if !mustHold(t, store, hold.ID).Active() {
		t.Error("hold ended on a stale session list")
	}
}

// LOOM-265 review: a revocation's deadline bounds its wait for a task's
// hold lock: a sweep stuck detaching on a target that doesn't answer
// doesn't hold up a logout past it.
func TestEndSessionHolds_DeadlineBoundsTheLockWait(t *testing.T) {
	store, ws, exec, o, clock := sweepSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	takeOver(t, o, task.ID)
	clock.Advance(time.Minute)

	stuck, unstick := make(chan struct{}), make(chan struct{})
	var once sync.Once
	exec.mu.Lock()
	exec.onDetach = func() { once.Do(func() { close(stuck); <-unstick }) }
	exec.mu.Unlock()
	swept := make(chan int)
	go func() { swept <- orchestrator.NewHoldSweeper(o).Sweep(ctx) }()
	<-stuck

	deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := o.EndSessionHolds(deadline, "sess-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("EndSessionHolds behind a stuck detach = %v; want the deadline", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("EndSessionHolds waited %v", waited)
	}
	close(unstick)
	if n := <-swept; n != 1 {
		t.Errorf("sweep ended %d holds, want 1", n)
	}
}

// LOOM-265 review: a grace retried after a failure counts the lease from
// the first try, the start: it moves the holds the downtime left, never
// one taken since, whose lease already runs past it.
func TestHoldSweeper_GraceRetryCountsFromTheStart(t *testing.T) {
	store, ws, exec, _, clock := sweepSetup(t)
	ctx := context.Background()
	wrapped := &failingGraceStore{Store: store, fail: true}
	o := orchestrator.New(wrapped, exec.factory(), &hookDetector{}, orchestrator.WithClock(clock.Now))
	before := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	old := takeOver(t, o, before.ID)
	clock.Advance(10 * time.Minute) // the downtime
	start := clock.Now()
	sweeper := orchestrator.NewHoldSweeper(o)
	if _, err := sweeper.Grace(ctx); err == nil {
		t.Fatal("Grace succeeded on a failing store")
	}

	clock.Advance(30 * time.Second)
	after := taskIn(t, store, o, ws, "conv-2", registry.TaskStatusAwaitingInput, "")
	fresh, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: after.ID, SessionID: "sess-2"})
	if err != nil {
		t.Fatal(err)
	}
	wrapped.mu.Lock()
	wrapped.fail = false
	wrapped.mu.Unlock()
	n, err := sweeper.Grace(ctx)
	if err != nil || n != 1 {
		t.Fatalf("retried Grace = %d, %v; want only the hold the downtime left moved", n, err)
	}
	if h := mustHold(t, store, old.ID); !h.ExpiresAt.Equal(start.Add(45 * time.Second)) {
		t.Errorf("old hold expires %v, want the start + 45 s (%v)", h.ExpiresAt, start.Add(45*time.Second))
	}
	if h := mustHold(t, store, fresh.Hold.ID); !h.ExpiresAt.Equal(fresh.Hold.ExpiresAt) {
		t.Errorf("the hold taken since moved: %v, was %v", h.ExpiresAt, fresh.Hold.ExpiresAt)
	}
}
