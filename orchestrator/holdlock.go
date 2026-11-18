package orchestrator

import "sync"

// holdLocks serializes everything that takes, renews or ends a task's
// holds (LOOM-265): Takeover, Heartbeat, Release, the hold sweeper and
// session revocation. A hold's end is two steps, detaching the task's
// tmux clients and then the store write, and the detach can't be undone.
// Without the lock a takeover could land between them (its new holder's
// client detached, or its hold ended without a detach), and a heartbeat
// could renew a hold the sweeper had already judged expired. The store's
// own transactions keep each write consistent; this keeps the decisions
// and the detach that goes with them in one piece. One loomuxd owns a
// database, so a lock in the process is enough.
type holdLocks struct {
	mu    sync.Mutex
	tasks map[string]*taskHoldLock
}

type taskHoldLock struct {
	mu sync.Mutex
	// users counts the callers holding or waiting for mu, so the entry
	// is dropped once nobody needs it.
	users int
}

// lock locks taskID's holds and returns the unlock.
func (l *holdLocks) lock(taskID string) (unlock func()) {
	l.mu.Lock()
	if l.tasks == nil {
		l.tasks = map[string]*taskHoldLock{}
	}
	e := l.tasks[taskID]
	if e == nil {
		e = &taskHoldLock{}
		l.tasks[taskID] = e
	}
	e.users++
	l.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		e.users--
		if e.users == 0 {
			delete(l.tasks, taskID)
		}
		l.mu.Unlock()
	}
}
