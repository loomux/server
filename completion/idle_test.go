package completion_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// scriptedExecutor returns CapturePane outputs from a fixed sequence, one
// step per call; the last entry repeats once the sequence is exhausted.
// Every call signals captureCh, letting tests synchronize precisely on
// "a poll just happened" without any real sleeping.
type scriptedExecutor struct {
	mu        sync.Mutex
	outputs   []string
	calls     int
	captureCh chan struct{}
}

func newScriptedExecutor(outputs []string) *scriptedExecutor {
	return &scriptedExecutor{outputs: outputs, captureCh: make(chan struct{}, 1000)}
}

func (e *scriptedExecutor) NewSession(context.Context, string, string, string) error { return nil }
func (e *scriptedExecutor) HasSession(context.Context, string) (bool, error)         { return true, nil }
func (e *scriptedExecutor) SendKeys(context.Context, string, string, bool) error     { return nil }
func (e *scriptedExecutor) KillSession(context.Context, string) error                { return nil }
func (e *scriptedExecutor) Close() error                                             { return nil }

func (e *scriptedExecutor) CapturePane(context.Context, string) (string, error) {
	e.mu.Lock()
	i := e.calls
	if i >= len(e.outputs) {
		i = len(e.outputs) - 1
	}
	e.calls++
	out := e.outputs[i]
	e.mu.Unlock()
	e.captureCh <- struct{}{}
	return out, nil
}

var _ targets.TargetExecutor = (*scriptedExecutor)(nil)

func (e *scriptedExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// fakeClock is a minimal, hand-rolled completion.Clock for deterministic
// tests: Now() and ticker firing are both driven explicitly by test
// code via Advance, so idle-duration threshold logic can be tested
// exactly, with zero real sleeping.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*fakeTicker
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTicker(time.Duration) completion.Ticker {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTicker{ch: make(chan time.Time, 1)}
	c.tickers = append(c.tickers, t)
	return t
}

// Advance moves the fake clock forward by d and fires every registered
// ticker once.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	tickers := append([]*fakeTicker(nil), c.tickers...)
	c.mu.Unlock()

	for _, t := range tickers {
		select {
		case t.ch <- now:
		default:
		}
	}
}

var _ completion.Clock = (*fakeClock)(nil)

type fakeTicker struct {
	ch chan time.Time
}

func (t *fakeTicker) C() <-chan time.Time { return t.ch }
func (t *fakeTicker) Stop()               {}

var _ completion.Ticker = (*fakeTicker)(nil)

func TestIdleWatcher_FiresAfterConfiguredIdleWindow(t *testing.T) {
	// Output changes on the first poll-after-baseline, then holds steady
	// — idle should fire once simulated elapsed time since the last
	// change crosses idleTimeout.
	exec := newScriptedExecutor([]string{"output-a", "output-b", "output-b", "output-b"})
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }

	clk := newFakeClock(time.Unix(0, 0))
	w := completion.NewIdleWatcher(factory, time.Second, completion.WithClock(clk))

	task := &registry.Task{ID: "t1", TmuxSession: "sess"}
	target := &registry.Target{ID: "target-1", Kind: registry.TargetKindLocal}
	idleTimeout := 5 * time.Second

	errCh := make(chan error, 1)
	go func() { errCh <- w.Wait(context.Background(), task, target, idleTimeout) }()

	<-exec.captureCh // call 1: "output-a" — establishes baseline at t=0

	clk.Advance(1 * time.Second)
	<-exec.captureCh // call 2: "output-b" — changed, baseline resets to t=1s

	clk.Advance(2 * time.Second)
	<-exec.captureCh // call 3: "output-b" — unchanged, elapsed=2s < 5s idle timeout

	clk.Advance(3 * time.Second)
	<-exec.captureCh // call 4: "output-b" — unchanged, elapsed=5s >= 5s idle timeout: done

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Wait did not return after the idle threshold was crossed")
	}
}

func TestIdleWatcher_ContextCancelledIfNeverIdle(t *testing.T) {
	// Every poll returns different output, so the idle threshold is
	// never reached — Wait should only return once ctx is cancelled.
	exec := newScriptedExecutor([]string{"a", "b", "c", "d", "e", "f", "g", "h"})
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }

	clk := newFakeClock(time.Unix(0, 0))
	w := completion.NewIdleWatcher(factory, time.Second, completion.WithClock(clk))

	task := &registry.Task{ID: "t2", TmuxSession: "sess"}
	target := &registry.Target{ID: "target-1", Kind: registry.TargetKindLocal}
	idleTimeout := 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- w.Wait(ctx, task, target, idleTimeout) }()

	<-exec.captureCh
	clk.Advance(1 * time.Second)
	<-exec.captureCh
	clk.Advance(1 * time.Second)
	<-exec.captureCh

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait: err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Wait did not return after ctx was cancelled")
	}
}
