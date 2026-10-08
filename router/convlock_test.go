package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConvLocksSerializePerConversation(t *testing.T) {
	var c convLocks
	var inA, maxA atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := c.lock(context.Background(), "a")
			if err != nil {
				t.Error(err)
				return
			}
			defer unlock()
			n := inA.Add(1)
			if n > maxA.Load() {
				maxA.Store(n)
			}
			time.Sleep(2 * time.Millisecond)
			inA.Add(-1)
		}()
	}
	// Another conversation isn't held up by "a".
	done := make(chan struct{})
	go func() {
		if unlock, err := c.lock(context.Background(), "b"); err == nil {
			unlock()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("conversation b waited on a")
	}
	wg.Wait()
	if maxA.Load() != 1 {
		t.Errorf("%d turns in conversation a at once, want 1", maxA.Load())
	}
	if len(c.locks) != 0 {
		t.Errorf("locks left behind: %d", len(c.locks))
	}
}

// A waiter whose context ends gives up, holding nothing, rather than
// wait out the turn ahead of it (LOOM-165).
func TestConvLocksWaiterHonoursContext(t *testing.T) {
	var c convLocks
	unlock, err := c.lock(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if u, err := c.lock(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) || u != nil {
		t.Fatalf("lock while held = %v, %v; want DeadlineExceeded and no unlock", u != nil, err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("waited %s after the context ended", waited)
	}
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := c.lock(cancelled, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("lock with a cancelled context = %v, want Canceled", err)
	}
	if c.tryLock("a") != nil {
		t.Fatal("tryLock took a held lock")
	}
	unlock()
	// The given-up waiters left nothing behind: the lock is free and its
	// entry gone.
	if len(c.locks) != 0 {
		t.Fatalf("locks left behind: %d", len(c.locks))
	}
	again := c.tryLock("a")
	if again == nil {
		t.Fatal("lock not free after unlock")
	}
	again()
}
