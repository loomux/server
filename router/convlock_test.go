package router

import (
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
			defer c.lock("a")()
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
	go func() { c.lock("b")(); close(done) }()
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
