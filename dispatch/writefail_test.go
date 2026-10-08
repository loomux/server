package dispatch_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/registry"
)

// flakyStore fails TransitionDispatch to the statuses in failTo, failures
// times each (-1: always).
type flakyStore struct {
	dispatch.Store
	mu       sync.Mutex
	failTo   map[registry.DispatchStatus]int
	failures int
}

func (f *flakyStore) TransitionDispatch(ctx context.Context, d *registry.Dispatch, from registry.DispatchStatus) error {
	f.mu.Lock()
	left, ok := f.failTo[d.Status]
	if ok && left != 0 {
		f.failTo[d.Status] = left - 1
		f.failures++
		f.mu.Unlock()
		return errors.New("disk I/O error")
	}
	f.mu.Unlock()
	return f.Store.TransitionDispatch(ctx, d, from)
}

// LOOM-146: a status write that fails for a moment is retried, so the
// job finishes normally and its conversation is free again.
func TestTransientStatusWriteFailureIsRetried(t *testing.T) {
	store := &flakyStore{Store: newStore(t), failTo: map[registry.DispatchStatus]int{
		registry.DispatchStatusRunning: 2, registry.DispatchStatusSucceeded: 2,
	}}
	svc := newService(t, store, func(context.Context, *registry.Dispatch) (string, error) { return "ok", nil },
		dispatch.WithWriteRetry(5, time.Millisecond))
	d, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c", Message: "m"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := waitDone(t, svc, d.ID); got.Status != registry.DispatchStatusSucceeded {
		t.Fatalf("status = %s, want succeeded", got.Status)
	}
	if store.failures != 4 {
		t.Errorf("failed writes = %d, want 4 (all retried)", store.failures)
	}
	if _, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c", Message: "next"}); err != nil {
		t.Fatalf("the conversation is still busy: %v", err)
	}
}

// LOOM-146: a status write that keeps failing leaves the row unfinished
// with no job; the sweep marks it interrupted, freeing the conversation
// without a restart.
func TestSweepOrphansFreesAConversationWhoseResultWasLost(t *testing.T) {
	store := &flakyStore{Store: newStore(t), failTo: map[registry.DispatchStatus]int{registry.DispatchStatusSucceeded: -1}}
	svc := newService(t, store, func(context.Context, *registry.Dispatch) (string, error) { return "ok", nil },
		dispatch.WithWriteRetry(3, time.Millisecond))
	ctx := context.Background()
	d, err := svc.Submit(ctx, dispatch.Request{ConversationID: "c", Message: "m"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitDone(t, svc, d.ID)

	var busy *dispatch.BusyError
	if _, err := svc.Submit(ctx, dispatch.Request{ConversationID: "c", Message: "next"}); !errors.As(err, &busy) {
		t.Fatalf("Submit after the lost result: %v, want BusyError (the bug being swept up)", err)
	}
	n, err := svc.SweepOrphans(ctx)
	if err != nil || n != 1 {
		t.Fatalf("SweepOrphans = %d, %v; want 1", n, err)
	}
	got, err := svc.Get(ctx, d.ID)
	if err != nil || got.Status != registry.DispatchStatusInterrupted {
		t.Fatalf("after the sweep: %+v, %v; want interrupted", got, err)
	}
	if _, err := svc.Submit(ctx, dispatch.Request{ConversationID: "c", Message: "next"}); err != nil {
		t.Fatalf("Submit after the sweep: %v", err)
	}
}

// The sweep never touches a job that is still running here, and does
// nothing once shutdown has begun (those rows are Recover's).
func TestSweepOrphansLeavesLiveJobsAndShutdownAlone(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	svc := dispatch.New(newStore(t), func(ctx context.Context, _ *registry.Dispatch) (string, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "ok", nil
	})
	ctx := context.Background()
	d, err := svc.Submit(ctx, dispatch.Request{ConversationID: "c", Message: "m"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started
	if n, err := svc.SweepOrphans(ctx); err != nil || n != 0 {
		t.Fatalf("SweepOrphans with a live job = %d, %v; want 0", n, err)
	}

	shutCtx, cancel := context.WithCancel(ctx)
	cancel() // interrupt at once: the row is left running for Recover
	_ = svc.Shutdown(shutCtx)
	if n, err := svc.SweepOrphans(ctx); err != nil || n != 0 {
		t.Fatalf("SweepOrphans after shutdown = %d, %v; want 0", n, err)
	}
	if got, err := svc.Get(ctx, d.ID); err != nil || got.Status != registry.DispatchStatusRunning {
		t.Fatalf("after shutdown and sweep: %+v, %v; want running (for Recover)", got, err)
	}
	close(release)
}
