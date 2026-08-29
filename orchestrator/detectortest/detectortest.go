// Package detectortest provides a trivial CompletionDetector stand-in for
// tests: ManualDetector.Wait blocks until Signal is called for the same
// task, or the context is done. The real tiered detector (native
// hooks/self-report/idle heuristic, design spec §5) is a separate concern
// (LOOM-6) — orchestrator code should never assume which strategy is
// behind the seam, so tests exercise it via explicit manual signaling
// instead.
package detectortest

import (
	"context"
	"sync"

	"github.com/Loomux/server/registry"
)

// ManualDetector is a CompletionDetector whose completion signals are
// driven explicitly by test code via Signal, rather than by any real
// detection strategy.
type ManualDetector struct {
	mu    sync.Mutex
	chans map[string]chan struct{}
}

// NewManualDetector constructs a ManualDetector.
func NewManualDetector() *ManualDetector {
	return &ManualDetector{chans: make(map[string]chan struct{})}
}

// Wait blocks until Signal(task.ID) is called, or ctx is done.
func (d *ManualDetector) Wait(ctx context.Context, task *registry.Task) error {
	ch := d.chanFor(task.ID)
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Signal unblocks any current or future Wait call for taskID. Safe to
// call before Wait is called, and safe to call more than once.
func (d *ManualDetector) Signal(taskID string) {
	ch := d.chanFor(taskID)
	select {
	case <-ch:
		// already signaled
	default:
		close(ch)
	}
}

func (d *ManualDetector) chanFor(taskID string) chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	ch, ok := d.chans[taskID]
	if !ok {
		ch = make(chan struct{})
		d.chans[taskID] = ch
	}
	return ch
}
