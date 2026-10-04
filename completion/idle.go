package completion

import (
	"context"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// Clock abstracts time so IdleWatcher's idle-duration logic is testable
// deterministically, with zero real sleeping. realClock (the default) is
// backed by the standard library; tests inject a fake via WithClock.
type Clock interface {
	Now() time.Time
	NewTicker(d time.Duration) Ticker
}

// Ticker abstracts *time.Ticker.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTicker(d time.Duration) Ticker {
	return &realTicker{t: time.NewTicker(d)}
}

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }

// IdleWatcher implements design spec §5's tier 3 (idle-time heuristic):
// poll a task's pane and treat "no change for at least idleTimeout" as
// completion. The true last resort — explicitly the weakest signal in
// the system, not a default. Unlike MarkerWatcher, Wait here takes extra
// parameters beyond what orchestrator.CompletionDetector requires (the
// resolved target and a per-call idle timeout), so IdleWatcher does not
// itself implement that interface — only the top-level Detector does,
// dispatching to this with the resolved per-agent-type configuration.
type IdleWatcher struct {
	newExecutor  orchestrator.ExecutorFactory
	pollInterval time.Duration
	clock        Clock
}

// IdleWatcherOption configures an IdleWatcher.
type IdleWatcherOption func(*IdleWatcher)

// WithClock overrides the Clock used for timing. Test-only hook, mirrors
// LOOM-4's RemoteOption pattern (targets.WithPort etc).
func WithClock(c Clock) IdleWatcherOption {
	return func(w *IdleWatcher) { w.clock = c }
}

// NewIdleWatcher constructs an IdleWatcher.
func NewIdleWatcher(newExecutor orchestrator.ExecutorFactory, pollInterval time.Duration, opts ...IdleWatcherOption) *IdleWatcher {
	w := &IdleWatcher{newExecutor: newExecutor, pollInterval: pollInterval, clock: realClock{}}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Wait blocks until target's pane output for task hasn't changed for at
// least idleTimeout, or ctx is done.
func (w *IdleWatcher) Wait(ctx context.Context, task *registry.Task, target *registry.Target, idleTimeout time.Duration) error {
	exec, err := w.newExecutor(target)
	if err != nil {
		return err
	}

	var lastOutput string
	var lastChange time.Time
	haveBaseline := false

	ticker := w.clock.NewTicker(w.pollInterval)
	defer ticker.Stop()

	var down outage
	for {
		output, err := exec.CapturePane(ctx, task.TmuxSession)
		if err != nil {
			if !down.tolerate(err) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C():
			}
			continue
		}
		down.clear()

		now := w.clock.Now()
		if !haveBaseline || output != lastOutput {
			lastOutput = output
			lastChange = now
			haveBaseline = true
		} else if now.Sub(lastChange) >= idleTimeout {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C():
		}
	}
}
