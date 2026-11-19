package orchestrator

import (
	"context"
	"sync"
)

// turnWaits are the waits in progress on tasks' running turns, by task
// (LOOM-266), so that a takeover can end the wait on its task's turn at
// once rather than when the agent finishes.
//
// WaitForCompletion adds its wait before it reads the task, and Takeover
// ends the task's waits after its status write. So a takeover racing a
// wait's start is always seen by one of them: either the wait reads the
// task already taken over, or the wait was added before the takeover
// wrote and is ended by it.
type turnWaits struct {
	mu    sync.Mutex
	tasks map[string]map[*turnWait]struct{}
}

type turnWait struct {
	cancel context.CancelCauseFunc
}

// add registers a wait on taskID's turn and returns its context, derived
// from ctx, and the func that unregisters it (and releases the context).
func (w *turnWaits) add(ctx context.Context, taskID string) (context.Context, func()) {
	waitCtx, cancel := context.WithCancelCause(ctx)
	tw := &turnWait{cancel: cancel}
	w.mu.Lock()
	if w.tasks == nil {
		w.tasks = map[string]map[*turnWait]struct{}{}
	}
	if w.tasks[taskID] == nil {
		w.tasks[taskID] = map[*turnWait]struct{}{}
	}
	w.tasks[taskID][tw] = struct{}{}
	w.mu.Unlock()
	return waitCtx, func() {
		w.mu.Lock()
		delete(w.tasks[taskID], tw)
		if len(w.tasks[taskID]) == 0 {
			delete(w.tasks, taskID)
		}
		w.mu.Unlock()
		cancel(nil)
	}
}

// end cancels every wait on taskID's turn with cause, and returns how
// many there were.
func (w *turnWaits) end(taskID string, cause error) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	for tw := range w.tasks[taskID] {
		tw.cancel(cause)
	}
	return len(w.tasks[taskID])
}
