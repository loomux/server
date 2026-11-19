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

// LOOM-266 (TUI S5): taking over a running turn ends the turn's wait at
// once, as taken_over (docs/design/tui-client-design.md, "Taking over a
// running turn").

// blockingDetector is a CompletionDetector whose turns never finish by
// themselves: Wait blocks until its context ends, as the real detector
// does on an agent still working. It says which task each wait is for.
type blockingDetector struct {
	entered chan string
}

func newBlockingDetector() *blockingDetector {
	return &blockingDetector{entered: make(chan string, 64)}
}

func (d *blockingDetector) Wait(ctx context.Context, task *registry.Task) error {
	d.entered <- task.ID
	<-ctx.Done()
	return ctx.Err()
}

// waitResult runs WaitForCompletion for taskID and returns where its
// result will arrive.
func waitResult(ctx context.Context, o *orchestrator.Orchestrator, taskID string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- o.WaitForCompletion(ctx, taskID) }()
	return done
}

// requireTakenOver checks a wait ended by a takeover: ErrTakenOver, and
// ErrStatusChanged with it, so every caller that leaves a changed task
// as it is (no failure written, no interrupt keys) does so here too.
func requireTakenOver(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, orchestrator.ErrTakenOver) {
		t.Fatalf("err = %v, want ErrTakenOver", err)
	}
	if !errors.Is(err, orchestrator.ErrStatusChanged) {
		t.Fatalf("err = %v, want it to be ErrStatusChanged too", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v: a takeover isn't the caller's context ending", err)
	}
}

func setupBlocking(t *testing.T) (registry.Store, *registry.Workspace, *fakeExecutor, *blockingDetector, *orchestrator.Orchestrator) {
	t.Helper()
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	exec := newFakeExecutor()
	detector := newBlockingDetector()
	return store, ws, exec, detector, orchestrator.New(store, exec.factory(), detector)
}

// A takeover of a running task ends the wait on its turn at once, rather
// than when the agent finishes, and the task stays human-takeover.
func TestTakeover_RunningTurnEndsItsWait(t *testing.T) {
	store, ws, _, detector, o := setupBlocking(t)
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusRunning, "")

	done := waitResult(context.Background(), o, task.ID)
	<-detector.entered
	select {
	case err := <-done:
		t.Fatalf("the wait ended before any takeover: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	hold := takeOver(t, o, task.ID)
	select {
	case err := <-done:
		requireTakenOver(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the turn's wait didn't end when its task was taken over")
	}
	got := mustTask(t, store, task.ID)
	if got.Status != registry.TaskStatusHumanTakeover || got.FailureReason != "" || got.ErrorClass != "" {
		t.Fatalf("task = %s (reason %q, class %q), want human-takeover with nothing failed", got.Status, got.FailureReason, got.ErrorClass)
	}
	if h, err := store.GetActiveDriveHold(context.Background(), task.ID); err != nil || h.ID != hold.ID {
		t.Fatalf("active hold = %+v, %v; want the takeover's", h, err)
	}
}

// A takeover ends only the taken task's wait: a turn on another task
// carries on.
func TestTakeover_EndsOnlyThatTasksWait(t *testing.T) {
	store, ws, _, detector, o := setupBlocking(t)
	taken := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusRunning, "")
	other := taskIn(t, store, o, ws, "conv-2", registry.TaskStatusRunning, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	takenDone := waitResult(ctx, o, taken.ID)
	otherDone := waitResult(ctx, o, other.ID)
	<-detector.entered
	<-detector.entered

	takeOver(t, o, taken.ID)
	select {
	case err := <-takenDone:
		requireTakenOver(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the taken task's wait didn't end")
	}
	select {
	case err := <-otherDone:
		t.Fatalf("another task's wait ended with the takeover: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if got := mustTask(t, store, other.ID); got.Status != registry.TaskStatusRunning {
		t.Fatalf("the other task is %s, want still running", got.Status)
	}
	cancel()
	if err := <-otherDone; !errors.Is(err, context.Canceled) || errors.Is(err, orchestrator.ErrTakenOver) {
		t.Fatalf("the other wait, its context cancelled = %v; want context.Canceled, not taken over", err)
	}
}

// Taking an awaiting-input or needs-attention task, or one another
// session holds, ends no wait: there is no turn in flight on it, and a
// wait on another task is left alone.
func TestTakeover_OfATaskWithNoTurnEndsNoWait(t *testing.T) {
	store, ws, _, detector, o := setupBlocking(t)
	running := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusRunning, "")
	done := waitResult(context.Background(), o, running.ID)
	<-detector.entered

	for i, status := range []registry.TaskStatus{registry.TaskStatusAwaitingInput, registry.TaskStatusNeedsAttention} {
		idle := taskIn(t, store, o, ws, fmt.Sprintf("conv-idle-%d", i), status, "")
		takeOver(t, o, idle.ID)
	}
	select {
	case err := <-done:
		t.Fatalf("a takeover of another task ended this wait: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	takeOver(t, o, running.ID)
	select {
	case err := <-done:
		requireTakenOver(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the running task's wait didn't end when it was taken over")
	}
}

// A task already taken over when its wait starts (taken between the
// turn's send and its wait) isn't waited on: the wait reports the
// takeover at once.
func TestWaitForCompletion_TakenOverBeforeItStarts(t *testing.T) {
	store, ws, _, detector, o := setupBlocking(t)
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusRunning, "")
	takeOver(t, o, task.ID)

	select {
	case err := <-waitResult(context.Background(), o, task.ID):
		requireTakenOver(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a wait on a task already taken over didn't end")
	}
	select {
	case <-detector.entered:
		t.Fatal("WaitForCompletion waited on a task already taken over")
	default:
	}
}

// A takeover racing the wait's start never leaves the wait running on: it
// is either read before the wait starts or ends the wait once started.
func TestWaitForCompletion_TakeoverRacingItsStart(t *testing.T) {
	store, ws, _, _, o := setupBlocking(t)
	for i := range 40 {
		task := taskIn(t, store, o, ws, fmt.Sprintf("conv-%d", i), registry.TaskStatusRunning, "")
		var wg sync.WaitGroup
		wg.Add(1)
		done := make(chan error, 1)
		go func() {
			defer wg.Done()
			done <- o.WaitForCompletion(context.Background(), task.ID)
		}()
		if _, err := o.Takeover(context.Background(), orchestrator.TakeoverRequest{TaskID: task.ID, SessionID: "sess-1"}); err != nil {
			t.Fatalf("Takeover: %v", err)
		}
		select {
		case err := <-done:
			requireTakenOver(t, err)
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the wait outlived the takeover that raced its start", i)
		}
		wg.Wait()
	}
}

// The caller's own context ending is not a takeover.
func TestWaitForCompletion_CallerCancelIsNotTakenOver(t *testing.T) {
	store, ws, _, detector, o := setupBlocking(t)
	task := taskIn(t, store, o, ws, "conv-1", registry.TaskStatusRunning, "")
	ctx, cancel := context.WithCancel(context.Background())
	done := waitResult(ctx, o, task.ID)
	<-detector.entered
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) || errors.Is(err, orchestrator.ErrTakenOver) || errors.Is(err, orchestrator.ErrStatusChanged) {
		t.Fatalf("err = %v, want context.Canceled alone", err)
	}
	if got := mustTask(t, store, task.ID); got.Status != registry.TaskStatusRunning {
		t.Fatalf("task = %s, want still running", got.Status)
	}
}

// A turn whose task changed some other way while it waited (a cancel
// failed it) lost its compare-and-set, but wasn't taken over.
func TestWaitForCompletion_OtherChangeIsNotTakenOver(t *testing.T) {
	f := setupCAS(t)
	task := f.launch(t, registry.TaskKindAgent, registry.TaskStatusRunning)
	f.detector.during = func() { f.setStatus(t, task.ID, registry.TaskStatusFailed) }

	err := f.o.WaitForCompletion(context.Background(), task.ID)
	if !errors.Is(err, orchestrator.ErrStatusChanged) || errors.Is(err, orchestrator.ErrTakenOver) {
		t.Fatalf("err = %v, want ErrStatusChanged without ErrTakenOver", err)
	}
}

// Every status write a turn makes after its wait or an external call
// that loses its compare-and-set to a takeover says so, so the turn ends
// as taken_over whichever step it was at: the wait's own end (the agent
// finished as it was taken), the send, a prompt or a failure found as
// the wait ended, and the answer to a prompt.
func TestLostWritesToATakeoverAreTakenOver(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		from registry.TaskStatus
		run  func(f *casFixture, taskID string) error
	}{
		{"WaitForCompletion", registry.TaskStatusRunning, func(f *casFixture, taskID string) error {
			f.detector.during = func() { f.setStatus(t, taskID, registry.TaskStatusHumanTakeover) }
			return f.o.WaitForCompletion(ctx, taskID)
		}},
		{"SendMessage", registry.TaskStatusAwaitingInput, func(f *casFixture, taskID string) error {
			f.exec.onPaste = func() error { f.setStatus(t, taskID, registry.TaskStatusHumanTakeover); return nil }
			return f.o.SendMessage(ctx, taskID, "hello")
		}},
		{"NeedAttention", registry.TaskStatusRunning, func(f *casFixture, taskID string) error {
			f.setStatus(t, taskID, registry.TaskStatusHumanTakeover)
			return f.o.NeedAttention(ctx, taskID, registry.TaskStatusRunning, &registry.Attention{Kind: registry.AttentionPermission, Title: "Run ls?"})
		}},
		{"Fail", registry.TaskStatusRunning, func(f *casFixture, taskID string) error {
			f.setStatus(t, taskID, registry.TaskStatusHumanTakeover)
			return f.o.Fail(ctx, taskID, registry.TaskStatusRunning, registry.TaskFailure{Class: registry.ErrorClassTimeout, Reason: "timed out"})
		}},
		{"Resume", registry.TaskStatusNeedsAttention, func(f *casFixture, taskID string) error {
			f.setStatus(t, taskID, registry.TaskStatusHumanTakeover)
			return f.o.Resume(ctx, taskID)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setupCAS(t)
			task := f.launch(t, registry.TaskKindAgent, c.from)
			err := c.run(f, task.ID)
			requireTakenOver(t, err)
			f.requireLeft(t, err, task.ID, registry.TaskStatusHumanTakeover, registry.WorkspaceStatusActive)
		})
	}
}

// Review (LOOM-266, round 1): the takeover is known from the turn's own
// step, not from re-reading the task after it, so a hold released at
// once (before the step ends) still ends the turn as taken over, and the
// step writes nothing over the handed-back task.

// takeOverAndRelease takes taskID over and hands it straight back.
func takeOverAndRelease(t *testing.T, o *orchestrator.Orchestrator, taskID string) {
	t.Helper()
	hold := takeOver(t, o, taskID)
	if _, err := o.Release(context.Background(), taskID, hold.ID); err != nil {
		t.Errorf("Release: %v", err)
	}
}

// The wait's detector finds the turn's end (or a prompt, or a timeout)
// just as the task is taken over and handed back.
func TestWaitForCompletion_TakeoverAndReleaseAsItEnds(t *testing.T) {
	for name, detErr := range map[string]error{
		"finished":  nil,
		"prompt":    &orchestrator.NeedsAttentionError{Attention: &registry.Attention{Kind: registry.AttentionPermission, Title: "Run ls?"}},
		"timed out": &orchestrator.TurnTimeoutError{Reason: orchestrator.TimeoutMaxTurnDuration, Limit: time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			f := setupCAS(t)
			task := f.launch(t, registry.TaskKindAgent, registry.TaskStatusRunning)
			f.detector.err = detErr
			f.detector.during = func() { takeOverAndRelease(t, f.o, task.ID) }

			err := f.o.WaitForCompletion(context.Background(), task.ID)
			requireTakenOver(t, err)
			if got := f.task(t, task.ID); got.Status != registry.TaskStatusAwaitingInput {
				t.Fatalf("task = %s, want awaiting-input as the release left it", got.Status)
			}
		})
	}
}

// The paste is in flight when the task is taken over: the send ends taken
// over, and writes nothing, even if the hold was released before the
// paste returned (the status read back would be awaiting-input again)
// and even if the paste failed (the task isn't failed for it).
func TestSendMessage_TakeoverDuringThePaste(t *testing.T) {
	for _, c := range []struct {
		name     string
		release  bool
		pasteErr error
	}{
		{"held", false, nil},
		{"released", true, nil},
		{"held, paste failed", false, errors.New("tmux: paste failed")},
		{"released, paste failed", true, errors.New("tmux: paste failed")},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setupCAS(t)
			task := f.launch(t, registry.TaskKindAgent, registry.TaskStatusAwaitingInput)
			f.exec.onPaste = func() error {
				if c.release {
					takeOverAndRelease(t, f.o, task.ID)
				} else {
					takeOver(t, f.o, task.ID)
				}
				return c.pasteErr
			}

			err := f.o.SendMessage(context.Background(), task.ID, "hello")
			requireTakenOver(t, err)
			want := registry.TaskStatusHumanTakeover
			if c.release {
				want = registry.TaskStatusAwaitingInput
			}
			got := f.task(t, task.ID)
			if got.Status != want || got.FailureReason != "" || got.ErrorClass != "" {
				t.Fatalf("task = %s (reason %q, class %q), want %s with nothing written by the send",
					got.Status, got.FailureReason, got.ErrorClass, want)
			}
		})
	}
}
