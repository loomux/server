package router

import "sync"

// convLocks serializes turns per conversation within this process
// (LOOM-83). Dispatch jobs already refuse a second in-flight message for
// a conversation; this is what stops any two callers of the router —
// a job and a resumed job, a test, a future client — from both launching
// a task or typing into one pane at once.
type convLocks struct {
	mu    sync.Mutex
	locks map[string]*convLock
}

type convLock struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until conversationID is free, and returns its unlock.
func (c *convLocks) lock(conversationID string) (unlock func()) {
	c.mu.Lock()
	if c.locks == nil {
		c.locks = make(map[string]*convLock)
	}
	l := c.locks[conversationID]
	if l == nil {
		l = &convLock{}
		c.locks[conversationID] = l
	}
	l.refs++
	c.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		c.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(c.locks, conversationID)
		}
		c.mu.Unlock()
	}
}
