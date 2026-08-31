package router_test

import (
	"context"
	"fmt"
	"sync"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// fakeSession records what was done to a session created by fakeExecutor.
type fakeSession struct {
	dir     string
	command string
	alive   bool
	keys    []string
}

// fakeExecutor is an in-memory targets.TargetExecutor for router's fast
// unit tests — no real tmux involved. When unreachable is true, every
// operation returns targets.ErrUnreachable.
type fakeExecutor struct {
	mu          sync.Mutex
	sessions    map[string]*fakeSession
	unreachable bool
	// capture is what CapturePane returns for every live session —
	// constant by design, so the idle-heuristic completion detector
	// treats it as "gone quiet" almost immediately.
	capture string
	// runOnceOutput/runOnceErr control RunOnce's return — used by the
	// version-check tests. Constant across calls; no per-command
	// scripting needed since a test only ever exercises one version
	// command at a time.
	runOnceOutput string
	runOnceErr    error
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{sessions: make(map[string]*fakeSession), capture: "fake pane output"}
}

func (e *fakeExecutor) factory() func(*registry.Target) (targets.TargetExecutor, error) {
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
	if _, ok := e.sessions[target]; !ok {
		return "", fmt.Errorf("fakeExecutor: no such session %q", target)
	}
	return e.capture, nil
}

func (e *fakeExecutor) KillSession(ctx context.Context, session string) error {
	if e.unreachable {
		return targets.ErrUnreachable
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

// FileExists and RemoveFile aren't exercised by router's fast unit
// tests (they always configure TierIdle via shortIdleAgentTypes);
// trivial stubs satisfy the interface.
func (e *fakeExecutor) FileExists(ctx context.Context, path string) (bool, error) { return false, nil }
func (e *fakeExecutor) RemoveFile(ctx context.Context, path string) error         { return nil }

func (e *fakeExecutor) RunOnce(ctx context.Context, command string) (string, error) {
	if e.unreachable {
		return "", targets.ErrUnreachable
	}
	return e.runOnceOutput, e.runOnceErr
}
