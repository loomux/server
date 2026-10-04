package dispatch_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newService builds a Service and shuts it down at cleanup, so no test
// leaks a runner goroutine past its own end.
func newService(t *testing.T, store dispatch.Store, run dispatch.RunFunc, opts ...dispatch.Option) *dispatch.Service {
	t.Helper()
	svc := dispatch.New(store, run, opts...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = svc.Shutdown(ctx)
	})
	return svc
}

func waitDone(t *testing.T, svc *dispatch.Service, id string) *registry.Dispatch {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := svc.Wait(ctx, id)
	if err != nil {
		t.Fatalf("Wait(%s): %v", id, err)
	}
	return d
}

func TestSubmitRunsJobAndPersistsUserMessage(t *testing.T) {
	store := newStore(t)
	var gotHint string
	svc := newService(t, store, func(ctx context.Context, d *registry.Dispatch) (string, error) {
		gotHint = d.WorkspaceHint
		return "reply to " + d.Message, nil
	})
	d, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "conv-1", Message: "hello", WorkspaceHint: "ws-1"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if d.ID == "" || d.ConversationID != "conv-1" {
		t.Fatalf("Submit = %+v", d)
	}
	msgs, err := store.ListMessagesByConversation(context.Background(), "conv-1")
	if err != nil || len(msgs) == 0 || msgs[0].Role != registry.MessageRoleUser || msgs[0].Content != "hello" || msgs[0].DispatchID != d.ID {
		t.Fatalf("user message not persisted at submit: %+v, %v", msgs, err)
	}

	done := waitDone(t, svc, d.ID)
	if done.Status != registry.DispatchStatusSucceeded || done.Reply != "reply to hello" || done.StartedAt == nil || done.FinishedAt == nil {
		t.Fatalf("finished dispatch = %+v", done)
	}
	if gotHint != "ws-1" {
		t.Errorf("run saw hint %q, want ws-1", gotHint)
	}
}

func TestSubmitMintsConversationID(t *testing.T) {
	svc := newService(t, newStore(t), func(context.Context, *registry.Dispatch) (string, error) { return "ok", nil })
	d, err := svc.Submit(context.Background(), dispatch.Request{Message: "hello"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if d.ConversationID == "" {
		t.Fatalf("no conversation id minted: %+v", d)
	}
}

func TestSubmitRejectsEmptyMessage(t *testing.T) {
	svc := newService(t, newStore(t), func(context.Context, *registry.Dispatch) (string, error) { return "", nil })
	if _, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c"}); !errors.Is(err, dispatch.ErrInvalidRequest) {
		t.Fatalf("Submit(empty) = %v, want ErrInvalidRequest", err)
	}
}

// The heart of LOOM-80: the submitter going away (a closed tab) must not
// reach the job.
func TestSubmitterCancellationDoesNotReachJob(t *testing.T) {
	release := make(chan struct{})
	jobCtxErr := make(chan error, 1)
	svc := newService(t, newStore(t), func(ctx context.Context, d *registry.Dispatch) (string, error) {
		<-release
		jobCtxErr <- ctx.Err()
		return "still here", nil
	})
	submitCtx, cancel := context.WithCancel(context.Background())
	d, err := svc.Submit(submitCtx, dispatch.Request{ConversationID: "c", Message: "m"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	cancel()
	close(release)
	done := waitDone(t, svc, d.ID)
	if done.Status != registry.DispatchStatusSucceeded || done.Reply != "still here" {
		t.Fatalf("dispatch after submitter cancelled = %+v", done)
	}
	if err := <-jobCtxErr; err != nil {
		t.Fatalf("job context was cancelled with the submitter: %v", err)
	}
}

func TestRunErrorFailsJobWithClass(t *testing.T) {
	svc := newService(t, newStore(t),
		func(context.Context, *registry.Dispatch) (string, error) { return "", errors.New("agent fell over") },
		dispatch.WithErrorClassifier(func(error) registry.ErrorClass { return registry.ErrorClassAgentExited }))
	d, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c", Message: "m"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := waitDone(t, svc, d.ID)
	if done.Status != registry.DispatchStatusFailed || done.Error != "agent fell over" || done.ErrorClass != registry.ErrorClassAgentExited {
		t.Fatalf("failed dispatch = %+v", done)
	}
}

func TestRunPanicFailsJob(t *testing.T) {
	svc := newService(t, newStore(t), func(context.Context, *registry.Dispatch) (string, error) { panic("kaboom") })
	d, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c", Message: "m"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := waitDone(t, svc, d.ID)
	if done.Status != registry.DispatchStatusFailed || done.ErrorClass != registry.ErrorClassInternal {
		t.Fatalf("panicking dispatch = %+v", done)
	}
}

func TestMaxDurationBoundsJob(t *testing.T) {
	svc := newService(t, newStore(t), func(ctx context.Context, d *registry.Dispatch) (string, error) {
		<-ctx.Done()
		return "", context.Cause(ctx)
	}, dispatch.WithMaxDuration(20*time.Millisecond))
	d, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c", Message: "m"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := waitDone(t, svc, d.ID)
	if done.Status != registry.DispatchStatusFailed || done.ErrorClass != registry.ErrorClassTimeout {
		t.Fatalf("over-long dispatch = %+v, want failed/timeout", done)
	}
}

func TestSameIdempotencyKeyRunsOnce(t *testing.T) {
	var runs atomic.Int32
	svc := newService(t, newStore(t), func(context.Context, *registry.Dispatch) (string, error) {
		runs.Add(1)
		return "once", nil
	})
	req := dispatch.Request{ConversationID: "c", Message: "m", IdempotencyKey: "k1"}
	a, err := svc.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit a: %v", err)
	}
	waitDone(t, svc, a.ID)
	b, err := svc.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit b: %v", err)
	}
	if b.ID != a.ID {
		t.Fatalf("retried submit made dispatch %s, want %s", b.ID, a.ID)
	}
	if runs.Load() != 1 {
		t.Fatalf("run called %d times, want 1", runs.Load())
	}
}

func TestConcurrentSameIdempotencyKeyRunsOnce(t *testing.T) {
	var runs atomic.Int32
	release := make(chan struct{})
	svc := newService(t, newStore(t), func(context.Context, *registry.Dispatch) (string, error) {
		runs.Add(1)
		<-release
		return "once", nil
	})
	req := dispatch.Request{ConversationID: "c", Message: "m", IdempotencyKey: "k-race"}
	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := svc.Submit(context.Background(), req)
			errs[i] = err
			if d != nil {
				ids[i] = d.ID
			}
		}()
	}
	wg.Wait()
	close(release)
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Submit %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("concurrent submits made different dispatches: %v", ids)
		}
	}
	waitDone(t, svc, ids[0])
	if runs.Load() != 1 {
		t.Fatalf("run called %d times, want 1", runs.Load())
	}
}

func TestReusedKeyWithDifferentRequest(t *testing.T) {
	svc := newService(t, newStore(t), func(context.Context, *registry.Dispatch) (string, error) { return "", nil })
	if _, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c", Message: "one", IdempotencyKey: "k"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	_, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c", Message: "two", IdempotencyKey: "k"})
	if !errors.Is(err, dispatch.ErrKeyReused) {
		t.Fatalf("Submit with reused key = %v, want ErrKeyReused", err)
	}
}

func TestBusyConversation(t *testing.T) {
	release := make(chan struct{})
	svc := newService(t, newStore(t), func(context.Context, *registry.Dispatch) (string, error) {
		<-release
		return "", nil
	})
	defer close(release)
	first, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c", Message: "one"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	_, err = svc.Submit(context.Background(), dispatch.Request{ConversationID: "c", Message: "two"})
	var busy *dispatch.BusyError
	if !errors.As(err, &busy) || busy.DispatchID != first.ID {
		t.Fatalf("second Submit = %v, want BusyError naming %s", err, first.ID)
	}
}

func TestWaitGivingUpLeavesJobRunning(t *testing.T) {
	release := make(chan struct{})
	svc := newService(t, newStore(t), func(context.Context, *registry.Dispatch) (string, error) {
		<-release
		return "late", nil
	})
	d, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c", Message: "m"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := svc.Wait(ctx, d.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want DeadlineExceeded", err)
	}
	close(release)
	if done := waitDone(t, svc, d.ID); done.Status != registry.DispatchStatusSucceeded || done.Reply != "late" {
		t.Fatalf("dispatch after a waiter gave up = %+v", done)
	}
}

func TestShutdownDrainsQuickJob(t *testing.T) {
	release := make(chan struct{})
	store := newStore(t)
	svc := dispatch.New(store, func(context.Context, *registry.Dispatch) (string, error) {
		<-release
		return "drained", nil
	})
	d, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c", Message: "m"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	go func() { time.Sleep(20 * time.Millisecond); close(release) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	got, _ := store.GetDispatch(context.Background(), d.ID)
	if got.Status != registry.DispatchStatusSucceeded || got.Reply != "drained" {
		t.Fatalf("drained dispatch = %+v", got)
	}
}

func TestShutdownInterruptsSlowJob(t *testing.T) {
	store := newStore(t)
	cause := make(chan error, 1)
	svc := dispatch.New(store, func(ctx context.Context, d *registry.Dispatch) (string, error) {
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return "", ctx.Err()
	})
	d, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c", Message: "m"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = svc.Shutdown(ctx)

	select {
	case c := <-cause:
		if !errors.Is(c, orchestrator.ErrInterrupted) {
			t.Fatalf("job context cause = %v, want orchestrator.ErrInterrupted", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("job context never cancelled")
	}
	got, _ := store.GetDispatch(context.Background(), d.ID)
	if got.Status != registry.DispatchStatusInterrupted || got.ErrorClass != registry.ErrorClassInterrupted || got.FinishedAt == nil {
		t.Fatalf("interrupted dispatch = %+v", got)
	}
	if _, err := svc.Submit(context.Background(), dispatch.Request{ConversationID: "c2", Message: "m"}); !errors.Is(err, dispatch.ErrShuttingDown) {
		t.Fatalf("Submit after Shutdown = %v, want ErrShuttingDown", err)
	}
	// A waiter on an interrupted job is released, not left hanging.
	if w := waitDone(t, svc, d.ID); w.Status != registry.DispatchStatusInterrupted {
		t.Fatalf("Wait on interrupted job = %+v", w)
	}
}

func TestRecoverMarksLeftoverJobsInterrupted(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	for _, id := range []string{"left-q", "left-r", "done"} {
		d := &registry.Dispatch{ID: id, ConversationID: "conv-" + id, Message: "m", RequestHash: "h", Status: registry.DispatchStatusQueued}
		if err := store.CreateDispatch(ctx, d, nil); err != nil {
			t.Fatalf("CreateDispatch: %v", err)
		}
	}
	r, _ := store.GetDispatch(ctx, "left-r")
	r.Status = registry.DispatchStatusRunning
	_ = store.TransitionDispatch(ctx, r, registry.DispatchStatusQueued)
	s, _ := store.GetDispatch(ctx, "done")
	s.Status = registry.DispatchStatusSucceeded
	_ = store.TransitionDispatch(ctx, s, registry.DispatchStatusQueued)

	svc := newService(t, store, func(context.Context, *registry.Dispatch) (string, error) { return "", nil })
	n, err := svc.Recover(ctx)
	if err != nil || n != 2 {
		t.Fatalf("Recover = %d, %v; want 2", n, err)
	}
	for _, id := range []string{"left-q", "left-r"} {
		got, _ := store.GetDispatch(ctx, id)
		if got.Status != registry.DispatchStatusInterrupted || got.ErrorClass != registry.ErrorClassInterrupted || got.Error == "" {
			t.Errorf("%s after Recover = %+v", id, got)
		}
	}
	if got, _ := store.GetDispatch(ctx, "done"); got.Status != registry.DispatchStatusSucceeded {
		t.Errorf("Recover touched a finished dispatch: %+v", got)
	}
}
