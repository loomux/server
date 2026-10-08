package app

import "sync"

// background runs the goroutines that outlive the call starting them but
// use the store — startup reconciliation, the sweepers, notifications —
// so Close can wait for them before it closes the store (LOOM-163):
// one still running then fails with "database is closed", and a
// notification still on its way is lost.
type background struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// Go runs f in a goroutine Close waits for, and reports true; once
// Close has begun, it runs nothing and reports false.
func (b *background) Go(f func()) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.wg.Go(f)
	return true
}

// close refuses any more goroutines and waits for those running. Each
// is bounded by a context of its own, or one the caller cancels first.
func (b *background) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.wg.Wait()
}
