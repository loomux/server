package router

import (
	"context"
	"sync"
)

// convLocks serializes turns per conversation within this process
// (LOOM-83). Dispatch jobs already refuse a second in-flight message for
// a conversation; this is what stops any two callers of the router —
// a job and a resumed job, a test, a future client — from both launching
// a task or typing into one pane at once.
type convLocks struct {
	mu    sync.Mutex
	locks map[string]*convLock
}

// convLock is held while its channel holds a value: a channel rather
// than a mutex so a waiter can give up when its context ends (LOOM-165).
type convLock struct {
	held chan struct{}
	refs int
}

// ref returns conversationID's lock, counting the caller as a user of
// it until release.
func (c *convLocks) ref(conversationID string) *convLock {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks == nil {
		c.locks = make(map[string]*convLock)
	}
	l := c.locks[conversationID]
	if l == nil {
		l = &convLock{held: make(chan struct{}, 1)}
		c.locks[conversationID] = l
	}
	l.refs++
	return l
}

func (c *convLocks) release(conversationID string, l *convLock) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l.refs--; l.refs == 0 {
		delete(c.locks, conversationID)
	}
}

// lock blocks until conversationID is free, and returns its unlock; or,
// if ctx ends first, returns ctx's error and holds nothing: a cancelled
// or timed-out turn doesn't wait out the one ahead of it.
func (c *convLocks) lock(ctx context.Context, conversationID string) (unlock func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l := c.ref(conversationID)
	select {
	case l.held <- struct{}{}:
	case <-ctx.Done():
		c.release(conversationID, l)
		return nil, ctx.Err()
	}
	return func() {
		<-l.held
		c.release(conversationID, l)
	}, nil
}

// tryLock takes conversationID's lock if it is free, returning its
// unlock, or nil if a turn holds it.
func (c *convLocks) tryLock(conversationID string) (unlock func()) {
	l := c.ref(conversationID)
	select {
	case l.held <- struct{}{}:
	default:
		c.release(conversationID, l)
		return nil
	}
	return func() {
		<-l.held
		c.release(conversationID, l)
	}
}
