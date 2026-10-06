package orchestrator_test

import (
	"context"
	"fmt"
	"sync"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// fakeSession records what was done to a session created by fakeExecutor,
// for test assertions.
type fakeSession struct {
	dir     string
	command string
	alive   bool
	keys    []string
}

// fakeExecutor is an in-memory targets.TargetExecutor for orchestrator's
// fast unit tests — no real tmux involved. When unreachable is true,
// every operation returns targets.ErrUnreachable, simulating a target
// that can't be reached.
type fakeExecutor struct {
	mu          sync.Mutex
	sessions    map[string]*fakeSession
	unreachable bool
	// onKill, if set, runs after KillSession succeeds — a test's hook for
	// something happening concurrently with a reap.
	onKill func()
	// runOnceOut is what RunOnce returns (the orphan sweep's session list).
	runOnceOut string
	// lastRunOnce is the last command RunOnce was given.
	lastRunOnce string
	// paneExit, if set, is every pane's exit: a paste is refused, as tmux
	// refuses one into a dead pane.
	paneExit *targets.PaneExit
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{sessions: make(map[string]*fakeSession)}
}

// factory returns an orchestrator.ExecutorFactory that always hands back
// this same fakeExecutor instance, regardless of target — mirrors
// production reality closely enough: a real RemoteExecutor's state lives
// at the OS level (its ControlPath), not in the Go struct, so a fresh
// instance per call still shares state; here that's simulated by sharing
// the one fake instance directly.
func (e *fakeExecutor) factory() orchestrator.ExecutorFactory {
	return func(*registry.Target) (targets.TargetExecutor, error) {
		return e, nil
	}
}

func (e *fakeExecutor) sessionFor(name string) *fakeSession {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessions[name]
}

func (e *fakeExecutor) NewSession(ctx context.Context, session, dir, command string) error {
	if e.unreachable {
		return targets.ErrUnreachable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sessions[session] = &fakeSession{dir: dir, command: command, alive: true}
	return nil
}

func (e *fakeExecutor) HasSession(ctx context.Context, session string) (bool, error) {
	if e.unreachable {
		return false, targets.ErrUnreachable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[session]
	return ok && s.alive, nil
}

func (e *fakeExecutor) SendKey(ctx context.Context, target, key string) error { return nil }

// PasteText records the text with the typed keys: to the tests both are
// what reached the pane.
func (e *fakeExecutor) PasteText(ctx context.Context, target, text string, enter bool) error {
	if len(text) > targets.MaxPasteBytes {
		return fmt.Errorf("fakeExecutor: %w", targets.ErrTextTooLarge)
	}
	if e.paneExit != nil {
		return fmt.Errorf("fakeExecutor: target pane has exited")
	}
	return e.SendKeys(ctx, target, text, enter)
}

func (e *fakeExecutor) SendKeys(ctx context.Context, target, keys string, enter bool) error {
	if e.unreachable {
		return targets.ErrUnreachable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[target]
	if !ok || !s.alive {
		return fmt.Errorf("fakeExecutor: no such session %q", target)
	}
	s.keys = append(s.keys, keys)
	return nil
}

func (e *fakeExecutor) CapturePane(ctx context.Context, target string) (string, error) {
	if e.unreachable {
		return "", targets.ErrUnreachable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[target]
	if !ok {
		return "", fmt.Errorf("fakeExecutor: no such session %q", target)
	}
	return fmt.Sprintf("%v", s.keys), nil
}

func (e *fakeExecutor) KillSession(ctx context.Context, session string) error {
	if e.unreachable {
		return targets.ErrUnreachable
	}
	if e.onKill != nil {
		defer e.onKill()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[session]
	if !ok {
		return fmt.Errorf("fakeExecutor: no such session %q", session)
	}
	s.alive = false
	return nil
}

func (e *fakeExecutor) Close() error { return nil }

// FileExists, RemoveFile, and RunOnce aren't exercised by this package's
// tests (orchestrator's own tests drive completion via detectortest.
// ManualDetector, never completion.Detector's marker tier, and never a
// router-level version check) — trivial stubs satisfy the interface.
func (e *fakeExecutor) FileExists(ctx context.Context, path string) (bool, error) { return false, nil }
func (e *fakeExecutor) RemoveFile(ctx context.Context, path string) error         { return nil }
func (e *fakeExecutor) RunOnce(ctx context.Context, command string) (string, error) {
	if e.unreachable {
		return "", targets.ErrUnreachable
	}
	e.mu.Lock()
	e.lastRunOnce = command
	e.mu.Unlock()
	return e.runOnceOut, nil
}

// PaneExited always reports the pane still running: this package's tests
// never exercise an exiting pane (completion.Detector does, LOOM-71).
func (e *fakeExecutor) PaneExited(ctx context.Context, target string) (*targets.PaneExit, error) {
	return e.paneExit, nil
}
