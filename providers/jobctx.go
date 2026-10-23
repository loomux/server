package providers

import (
	"context"
	"sync"
	"time"
)

// jobContext is a job's context (startJob): cancelled with the job, and
// done with context.DeadlineExceeded at its deadline, which extend
// pushes back while the plugin reports a step in progress, an image
// pull (LOOM-203). Done, Err and Deadline are its own; Value is the
// parent's.
type jobContext struct {
	context.Context
	mu       sync.Mutex
	done     chan struct{}
	err      error
	deadline time.Time
	timer    *time.Timer
	// base is the deadline as made, before any extendTo.
	base time.Time
}

// newJobContext is a jobContext under parent with timeout; it ends when
// parent does, with parent's error.
func newJobContext(parent context.Context, timeout time.Duration) *jobContext {
	at := time.Now().Add(timeout)
	c := &jobContext{Context: parent, done: make(chan struct{}), deadline: at, base: at}
	c.mu.Lock()
	c.timer = time.AfterFunc(timeout, c.expire)
	c.mu.Unlock()
	context.AfterFunc(parent, func() { c.finish(parent.Err()) })
	return c
}

func (c *jobContext) Deadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline, true
}

func (c *jobContext) Done() <-chan struct{} { return c.done }

func (c *jobContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// expire ends c at its deadline, or waits again for one pushed back.
func (c *jobContext) expire() {
	c.mu.Lock()
	if left := time.Until(c.deadline); left > 0 && c.err == nil {
		c.timer.Reset(left)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	c.finish(context.DeadlineExceeded)
}

func (c *jobContext) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	c.timer.Stop()
	close(c.done)
}

// remaining is the time left before the deadline.
func (c *jobContext) remaining() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Until(c.deadline)
}

// extendTo pushes the deadline back to at, but no further than limit
// past the deadline as made, unless c has ended.
func (c *jobContext) extendTo(at time.Time, limit time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if most := c.base.Add(limit); at.After(most) {
		at = most
	}
	if c.err == nil && at.After(c.deadline) {
		c.deadline = at
	}
}

// extended is how far extendTo has pushed the deadline back.
func (c *jobContext) extended() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline.Sub(c.base)
}
