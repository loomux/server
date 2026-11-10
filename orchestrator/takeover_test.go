package orchestrator_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// LOOM-264: takeover, heartbeat and release as holds
// (docs/design/tui-client-design.md, "Server awareness: holds").

// takeOver takes taskID for session "sess-1" and returns the hold.
func takeOver(t *testing.T, o *orchestrator.Orchestrator, taskID string) *registry.TaskHold {
	t.Helper()
	res, err := o.Takeover(context.Background(), orchestrator.TakeoverRequest{TaskID: taskID, SessionID: "sess-1", ClientLabel: "laptop"})
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	return res.Hold
}

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// holdSetup is setup with a fake clock on the orchestrator.
func holdSetup(t *testing.T) (registry.Store, *registry.Workspace, *fakeExecutor, *orchestrator.Orchestrator, *fakeClock) {
	t.Helper()
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	exec := newFakeExecutor()
	clock := newFakeClock()
	o := orchestrator.New(store, exec.factory(), &hookDetector{}, orchestrator.WithClock(clock.Now))
	return store, ws, exec, o, clock
}

// taskIn launches a task and puts it in status (with class, when failed).
func taskIn(t *testing.T, store registry.Store, o *orchestrator.Orchestrator, ws *registry.Workspace, conv string,
	status registry.TaskStatus, class registry.ErrorClass) *registry.Task {
	t.Helper()
	ctx := context.Background()
	task, err := o.Launch(ctx, ws.ID, conv, registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if status == registry.TaskStatusRunning {
		return task
	}
	task.Status, task.ErrorClass = status, class
	if status == registry.TaskStatusNeedsAttention {
		task.Attention = &registry.Attention{Kind: registry.AttentionPermission, Title: "Bash command"}
	}
	if status == registry.TaskStatusFailed || status == registry.TaskStatusCompleted {
		now := time.Now().UTC()
		task.CompletedAt = &now
	}
	if err := store.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	return task
}

func mustTask(t *testing.T, store registry.Store, id string) *registry.Task {
	t.Helper()
	task, err := store.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return task
}

// An unheld running, awaiting-input or needs-attention task moves to
// human-takeover with a hold that says who took it and from what.
func TestTakeover_UnheldTaskIsTaken(t *testing.T) {
	for _, status := range []registry.TaskStatus{registry.TaskStatusRunning, registry.TaskStatusAwaitingInput, registry.TaskStatusNeedsAttention} {
		t.Run(string(status), func(t *testing.T) {
			store, ws, exec, o, clock := holdSetup(t)
			ctx := context.Background()
			task := taskIn(t, store, o, ws, "conv-1", status, "")

			res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1", ClientKind: "tui", ClientLabel: "laptop"})
			if err != nil {
				t.Fatalf("Takeover: %v", err)
			}
			h := res.Hold
			if res.Renewed || res.Replaced != nil || h.TaskID != task.ID || h.SessionID != "sess-1" || h.ClientKind != "tui" || h.ClientLabel != "laptop" ||
				h.Mode != registry.HoldModeDrive || h.PrevStatus != status || h.TTL != orchestrator.DefaultHoldTTL ||
				!h.AcquiredAt.Equal(clock.Now()) || !h.ExpiresAt.Equal(clock.Now().Add(45*time.Second)) || !h.Active() {
				t.Errorf("result = %+v, hold %+v", res, h)
			}
			if res.Task.Status != registry.TaskStatusHumanTakeover {
				t.Errorf("returned task = %s", res.Task.Status)
			}
			stored := mustTask(t, store, task.ID)
			if stored.Status != registry.TaskStatusHumanTakeover {
				t.Fatalf("stored status = %s, want human-takeover", stored.Status)
			}
			if status == registry.TaskStatusNeedsAttention && stored.Attention == nil {
				t.Error("the prompt was cleared by the takeover; it is the release that clears it")
			}
			if active, err := store.GetActiveDriveHold(ctx, task.ID); err != nil || active.ID != h.ID {
				t.Errorf("active hold = %+v, %v", active, err)
			}
			// Automation is paused, and nothing was typed by the takeover.
			if err := o.SendMessage(ctx, task.ID, "echo hi"); !errors.Is(err, orchestrator.ErrHumanTakeover) {
				t.Errorf("SendMessage while held = %v, want ErrHumanTakeover", err)
			}
			if keys := exec.sessionFor(task.TmuxSession).keys; len(keys) != 0 {
				t.Errorf("keys sent: %v", keys)
			}
		})
	}
}

// The same session taking again gets its own hold back, renewed: how a
// restarted client recovers a task that is already human-takeover.
func TestTakeover_SameSessionRenews(t *testing.T) {
	store, ws, _, o, clock := holdSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	first, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1", TTL: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(20 * time.Second)
	// Force and another TTL change nothing about a hold that is its own.
	again, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1", TTL: 15 * time.Second, Force: true})
	if err != nil {
		t.Fatalf("second Takeover: %v", err)
	}
	if !again.Renewed || again.Hold.ID != first.Hold.ID || again.Replaced != nil {
		t.Fatalf("second takeover = %+v; want the first hold renewed", again)
	}
	if !again.Hold.RenewedAt.Equal(clock.Now()) || !again.Hold.ExpiresAt.Equal(clock.Now().Add(60*time.Second)) ||
		!again.Hold.AcquiredAt.Equal(first.Hold.AcquiredAt) || again.Hold.PrevStatus != registry.TaskStatusAwaitingInput {
		t.Errorf("renewed hold = %+v", again.Hold)
	}
	if holds, _ := store.ListActiveTaskHolds(ctx); len(holds) != 1 {
		t.Errorf("%d active holds, want 1", len(holds))
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("status = %s", got.Status)
	}
}

// Another session is refused with the holder's hold, unless it forces:
// then the old hold ends as taken_over and the task never leaves
// human-takeover.
func TestTakeover_HeldByAnotherSession(t *testing.T) {
	store, ws, _, o, clock := holdSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	first := takeOver(t, o, task.ID)

	_, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-2"})
	var held *orchestrator.TaskHeldError
	if !errors.As(err, &held) || held.Hold.ID != first.ID || held.Hold.SessionID != "sess-1" || held.Hold.ClientLabel != "laptop" {
		t.Fatalf("Takeover by another session = %v (%+v); want TaskHeldError with the holder's hold", err, held)
	}
	if h, _ := store.GetTaskHold(ctx, first.ID); !h.Active() {
		t.Fatal("the refused takeover ended the holder's hold")
	}

	clock.Advance(10 * time.Second)
	res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-2", ClientLabel: "phone", Force: true})
	if err != nil {
		t.Fatalf("forced Takeover: %v", err)
	}
	if res.Renewed || res.Hold.ID == first.ID || res.Hold.SessionID != "sess-2" || res.Hold.PrevStatus != registry.TaskStatusHumanTakeover {
		t.Errorf("forced takeover = %+v, hold %+v", res, res.Hold)
	}
	if res.Replaced == nil || res.Replaced.ID != first.ID || res.Replaced.EndedReason != registry.HoldEndTakenOver {
		t.Errorf("replaced = %+v; want the first hold, taken_over", res.Replaced)
	}
	old, _ := store.GetTaskHold(ctx, first.ID)
	if old.Active() || old.EndedReason != registry.HoldEndTakenOver || !old.EndedAt.Equal(clock.Now()) {
		t.Errorf("first hold = %+v; want ended as taken_over now", old)
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("status after a forced takeover = %s", got.Status)
	}

	// The loser's heartbeat learns why, and its release doesn't take the
	// task from the new holder.
	_, err = o.Heartbeat(ctx, task.ID, first.ID, "sess-1")
	var ended *orchestrator.HoldEndedError
	if !errors.As(err, &ended) || ended.Hold.EndedReason != registry.HoldEndTakenOver {
		t.Errorf("heartbeat of the replaced hold = %v; want HoldEndedError taken_over", err)
	}
	rel, err := o.Release(ctx, task.ID, first.ID)
	if err != nil || rel.Ended || rel.TaskReleased || rel.Hold.EndedReason != registry.HoldEndTakenOver {
		t.Errorf("release of the replaced hold = %+v, %v; want nothing done", rel, err)
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("status after the loser's release = %s; want human-takeover, the new holder's", got.Status)
	}
	if h, err := store.GetActiveDriveHold(ctx, task.ID); err != nil || h.ID != res.Hold.ID {
		t.Errorf("active hold = %+v, %v; want the new holder's", h, err)
	}
}

// A task in human-takeover with no hold (taken before holds existed) is
// taken where it stands.
func TestTakeover_UnheldHumanTakeover(t *testing.T) {
	store, ws, _, o, _ := holdSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusHumanTakeover, "")
	res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	if res.Hold.PrevStatus != registry.TaskStatusHumanTakeover || res.Task.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("hold %+v, task %s", res.Hold, res.Task.Status)
	}
	rel, err := o.Release(ctx, task.ID, res.Hold.ID)
	if err != nil || !rel.TaskReleased || rel.Task.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("release = %+v, %v", rel, err)
	}
}

// A failed task whose pane was kept for a person can be held, stays
// failed, and its release only ends the hold. Any other ended task
// can't be held.
func TestTakeover_EndedTasks(t *testing.T) {
	for _, tc := range []struct {
		status   registry.TaskStatus
		class    registry.ErrorClass
		holdable bool
	}{
		{registry.TaskStatusFailed, registry.ErrorClassLoginRequired, true},
		{registry.TaskStatusFailed, registry.ErrorClassAgentRateLimited, true},
		{registry.TaskStatusFailed, registry.ErrorClassAgentAPIError, true},
		{registry.TaskStatusFailed, registry.ErrorClassTimeout, false},
		{registry.TaskStatusFailed, registry.ErrorClassCancelled, false},
		{registry.TaskStatusFailed, "", false},
		{registry.TaskStatusCompleted, "", false},
	} {
		t.Run(fmt.Sprintf("%s_%s", tc.status, tc.class), func(t *testing.T) {
			store, ws, _, o, _ := holdSetup(t)
			ctx := context.Background()
			task := taskIn(t, store, o, ws, "conv-1", tc.status, tc.class)
			res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1"})
			if !tc.holdable {
				if !errors.Is(err, orchestrator.ErrTaskInactive) {
					t.Fatalf("Takeover = %v, want ErrTaskInactive", err)
				}
				if holds, _ := store.ListActiveTaskHolds(ctx); len(holds) != 0 {
					t.Errorf("a refused takeover left %d holds", len(holds))
				}
				return
			}
			if err != nil {
				t.Fatalf("Takeover: %v", err)
			}
			if res.Hold.PrevStatus != registry.TaskStatusFailed || res.Task.Status != registry.TaskStatusFailed {
				t.Errorf("hold %+v, task %s", res.Hold, res.Task.Status)
			}
			if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusFailed || got.ErrorClass != tc.class {
				t.Errorf("stored task = %s/%s; want it still failed as it was", got.Status, got.ErrorClass)
			}
			// Another session sees who is in the pane.
			var held *orchestrator.TaskHeldError
			if _, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-2"}); !errors.As(err, &held) {
				t.Errorf("second session on a held failed task = %v, want TaskHeldError", err)
			}
			rel, err := o.Release(ctx, task.ID, res.Hold.ID)
			if err != nil || !rel.Ended || rel.TaskReleased || rel.Task.Status != registry.TaskStatusFailed {
				t.Errorf("release = %+v, %v; want the hold ended and the task still failed", rel, err)
			}
		})
	}
}

// Watch isn't served yet and must never come back as a drive hold; an
// unknown mode, a TTL out of bounds and an unknown task are refused too.
// None of them writes anything.
func TestTakeover_Refusals(t *testing.T) {
	store, ws, _, o, _ := holdSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")

	if _, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1", Mode: registry.HoldModeWatch}); !errors.Is(err, orchestrator.ErrUnsupportedHoldMode) {
		t.Errorf("watch = %v, want ErrUnsupportedHoldMode", err)
	}
	var invalid *orchestrator.InvalidTakeoverError
	for name, req := range map[string]orchestrator.TakeoverRequest{
		"unknown mode":  {TaskID: task.ID, SessionID: "sess-1", Mode: "steer"},
		"ttl too short": {TaskID: task.ID, SessionID: "sess-1", TTL: 14 * time.Second},
		"ttl too long":  {TaskID: task.ID, SessionID: "sess-1", TTL: 301 * time.Second},
		"negative ttl":  {TaskID: task.ID, SessionID: "sess-1", TTL: -time.Second},
		"no session":    {TaskID: task.ID},
	} {
		if _, err := o.Takeover(ctx, req); !errors.As(err, &invalid) {
			t.Errorf("%s = %v, want InvalidTakeoverError", name, err)
		}
	}
	if _, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: "no-such", SessionID: "sess-1"}); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("unknown task = %v, want ErrNotFound", err)
	}
	if holds, _ := store.ListActiveTaskHolds(ctx); len(holds) != 0 {
		t.Errorf("refused takeovers left %d holds", len(holds))
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("status after refused takeovers = %s", got.Status)
	}
	// The bounds themselves are allowed.
	for _, ttl := range []time.Duration{orchestrator.MinHoldTTL, orchestrator.MaxHoldTTL} {
		res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1", TTL: ttl})
		if err != nil || res.Hold.TTL != ttl {
			t.Fatalf("Takeover with ttl %s = %+v, %v", ttl, res, err)
		}
		if _, err := o.Release(ctx, task.ID, res.Hold.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHeartbeat(t *testing.T) {
	store, ws, _, o, clock := holdSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	other := taskIn(t, store, o, ws, "conv-2", registry.TaskStatusAwaitingInput, "")
	hold := takeOver(t, o, task.ID)

	clock.Advance(15 * time.Second)
	h, err := o.Heartbeat(ctx, task.ID, hold.ID, "sess-1")
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !h.RenewedAt.Equal(clock.Now()) || !h.ExpiresAt.Equal(clock.Now().Add(orchestrator.DefaultHoldTTL)) || !h.Active() {
		t.Errorf("after heartbeat = %+v", h)
	}
	// A lease past its expiry that nothing has ended yet is still renewed:
	// only an ended hold is gone.
	clock.Advance(10 * time.Minute)
	if h, err := o.Heartbeat(ctx, task.ID, hold.ID, "sess-1"); err != nil || !h.ExpiresAt.Equal(clock.Now().Add(orchestrator.DefaultHoldTTL)) {
		t.Errorf("heartbeat past the expiry = %+v, %v", h, err)
	}

	if _, err := o.Heartbeat(ctx, task.ID, hold.ID, "sess-2"); !errors.Is(err, orchestrator.ErrHoldNotOwned) {
		t.Errorf("heartbeat by another session = %v, want ErrHoldNotOwned", err)
	}
	if _, err := o.Heartbeat(ctx, other.ID, hold.ID, "sess-1"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("heartbeat through another task = %v, want ErrNotFound", err)
	}
	if _, err := o.Heartbeat(ctx, task.ID, "no-such", "sess-1"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("heartbeat of an unknown hold = %v, want ErrNotFound", err)
	}

	if _, err := o.Release(ctx, task.ID, ""); err != nil { // taken back from elsewhere
		t.Fatal(err)
	}
	_, err = o.Heartbeat(ctx, task.ID, hold.ID, "sess-1")
	var ended *orchestrator.HoldEndedError
	if !errors.As(err, &ended) || ended.Hold.ID != hold.ID || ended.Hold.EndedReason != registry.HoldEndTakenBack {
		t.Errorf("heartbeat of an ended hold = %v; want HoldEndedError taken_back", err)
	}
}

// Release by hold id hands the task back, clears its stale prompt, and
// is idempotent; automated dispatch works again afterwards.
func TestRelease_ByHoldID(t *testing.T) {
	store, ws, exec, o, clock := holdSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusNeedsAttention, "")
	hold := takeOver(t, o, task.ID)

	clock.Advance(time.Minute)
	rel, err := o.Release(ctx, task.ID, hold.ID)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !rel.Ended || !rel.TaskReleased || rel.Hold.EndedReason != registry.HoldEndReleased || !rel.Hold.EndedAt.Equal(clock.Now()) {
		t.Errorf("release = %+v, hold %+v", rel, rel.Hold)
	}
	got := mustTask(t, store, task.ID)
	if got.Status != registry.TaskStatusAwaitingInput || got.Attention != nil {
		t.Errorf("task after release = %s, attention %+v; want awaiting-input with the prompt cleared", got.Status, got.Attention)
	}
	again, err := o.Release(ctx, task.ID, hold.ID)
	if err != nil || again.Ended || again.TaskReleased || again.Hold.EndedReason != registry.HoldEndReleased {
		t.Errorf("second release = %+v, %v; want the hold as it ended", again, err)
	}
	if err := o.SendMessage(ctx, task.ID, "echo released"); err != nil {
		t.Fatalf("SendMessage after release: %v", err)
	}
	if keys := exec.sessionFor(task.TmuxSession).keys; len(keys) != 1 || keys[0] != "echo released" {
		t.Errorf("keys = %v", keys)
	}
	if _, err := o.Release(ctx, task.ID, "no-such"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("release of an unknown hold = %v, want ErrNotFound", err)
	}
	if _, err := o.Release(ctx, "no-such-task", ""); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("release on an unknown task = %v, want ErrNotFound", err)
	}
}

// Release without a hold id is Take back: any session may, the hold ends
// as taken_back, and a task nobody holds is left alone.
func TestRelease_TakeBack(t *testing.T) {
	store, ws, _, o, _ := holdSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	hold := takeOver(t, o, task.ID)

	rel, err := o.Release(ctx, task.ID, "")
	if err != nil || !rel.Ended || !rel.TaskReleased || rel.Hold.ID != hold.ID || rel.Hold.EndedReason != registry.HoldEndTakenBack {
		t.Fatalf("take back = %+v, %v", rel, err)
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("status = %s", got.Status)
	}
	rel, err = o.Release(ctx, task.ID, "")
	if err != nil || rel.Hold != nil || rel.Ended || rel.TaskReleased {
		t.Errorf("take back with nothing held = %+v, %v; want nothing done", rel, err)
	}
	running := taskIn(t, store, o, ws, "conv-2", registry.TaskStatusRunning, "")
	if rel, err := o.Release(ctx, running.ID, ""); err != nil || rel.TaskReleased || rel.Task.Status != registry.TaskStatusRunning {
		t.Errorf("take back of a running task nobody holds = %+v, %v; want it left running", rel, err)
	}
}

// Taking over a running turn: the turn's own status write, after its
// wait, loses its compare-and-set (LOOM-262) and the task stays
// human-takeover.
func TestTakeover_RunningTurnIsNotOverwritten(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	exec := newFakeExecutor()
	detector := &hookDetector{}
	o := orchestrator.New(store, exec.factory(), detector)
	ctx := context.Background()
	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatal(err)
	}
	var hold *registry.TaskHold
	detector.during = func() { hold = takeOver(t, o, task.ID) }
	if err := o.WaitForCompletion(ctx, task.ID); !errors.Is(err, orchestrator.ErrStatusChanged) {
		t.Fatalf("WaitForCompletion across a takeover = %v, want ErrStatusChanged", err)
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Fatalf("status after the turn's wait ended = %s, want human-takeover", got.Status)
	}
	if hold == nil || hold.PrevStatus != registry.TaskStatusRunning {
		t.Errorf("hold = %+v; want prev_status running", hold)
	}
	if h, err := store.GetActiveDriveHold(ctx, task.ID); err != nil || h.ID != hold.ID {
		t.Errorf("active hold = %+v, %v", h, err)
	}
}

// Several sessions take the same task at once: one gets the hold, the
// others are told who has it.
func TestTakeover_Concurrent(t *testing.T) {
	store, ws, _, o, _ := holdSetup(t)
	ctx := context.Background()
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusAwaitingInput, "")
	const takers = 8
	type result struct {
		res *orchestrator.TakeoverResult
		err error
	}
	results := make(chan result, takers)
	for i := range takers {
		go func() {
			res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: fmt.Sprintf("sess-%d", i)})
			results <- result{res, err}
		}()
	}
	var winner *registry.TaskHold
	var losers []*orchestrator.TaskHeldError
	for range takers {
		r := <-results
		var held *orchestrator.TaskHeldError
		switch {
		case r.err == nil && winner == nil:
			winner = r.res.Hold
		case r.err == nil:
			t.Fatalf("two sessions got a hold: %s and %s", winner.SessionID, r.res.Hold.SessionID)
		case errors.As(r.err, &held):
			losers = append(losers, held)
		default:
			t.Fatalf("Takeover: %v", r.err)
		}
	}
	if winner == nil || len(losers) != takers-1 {
		t.Fatalf("winner %+v, %d losers; want one winner and %d told who has it", winner, len(losers), takers-1)
	}
	for _, l := range losers {
		if l.Hold.ID != winner.ID {
			t.Errorf("a loser was shown hold %s, want the winner's %s", l.Hold.ID, winner.ID)
		}
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("status = %s", got.Status)
	}
}

// meanwhileStore runs meanwhile once, between Takeover's read of the task
// and its write.
type meanwhileStore struct {
	registry.Store
	once      sync.Once
	meanwhile func()
}

func (s *meanwhileStore) GetActiveDriveHold(ctx context.Context, taskID string) (*registry.TaskHold, error) {
	h, err := s.Store.GetActiveDriveHold(ctx, taskID)
	s.once.Do(s.meanwhile)
	return h, err
}

// The task ends its turn between Takeover's read and its write: the
// takeover is a compare-and-set, so it looks again and takes the task
// from the status it has now, rather than writing over it.
func TestTakeover_StatusChangesMeanwhile(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	exec := newFakeExecutor()
	ctx := context.Background()
	wrapped := &meanwhileStore{Store: store}
	o := orchestrator.New(wrapped, exec.factory(), &hookDetector{})
	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatal(err)
	}
	wrapped.meanwhile = func() {
		cur := mustTask(t, store, task.ID)
		cur.Status = registry.TaskStatusAwaitingInput
		if applied, err := store.TransitionTask(ctx, cur, registry.TaskStatusRunning); err != nil || !applied {
			t.Errorf("meanwhile transition = %v, %v", applied, err)
		}
	}
	res, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	if res.Hold.PrevStatus != registry.TaskStatusAwaitingInput {
		t.Errorf("prev_status = %s; want awaiting-input, the status it was taken from", res.Hold.PrevStatus)
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("status = %s", got.Status)
	}

	// It completes meanwhile instead: the takeover is refused, nothing held.
	done, err := o.Launch(ctx, ws.ID, "conv-2", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatal(err)
	}
	wrapped.once = sync.Once{}
	wrapped.meanwhile = func() {
		if err := o.Complete(ctx, done.ID, registry.TaskStatusRunning, "done"); err != nil {
			t.Errorf("Complete: %v", err)
		}
	}
	if _, err := o.Takeover(ctx, orchestrator.TakeoverRequest{TaskID: done.ID, SessionID: "sess-1"}); !errors.Is(err, orchestrator.ErrTaskInactive) {
		t.Fatalf("Takeover of a task that completed meanwhile = %v, want ErrTaskInactive", err)
	}
	if got := mustTask(t, store, done.ID); got.Status != registry.TaskStatusCompleted {
		t.Errorf("status = %s; want completed, not written over", got.Status)
	}
	if _, err := store.GetActiveDriveHold(ctx, done.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("a hold was left on the completed task: %v", err)
	}
}
