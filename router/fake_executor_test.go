package router_test

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
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
	// fileExists controls FileExists's return for every path — a fast
	// unit test exercising TierMarker (LOOM-32) sets this true to
	// simulate "the hook already touched the marker," so
	// completion.MarkerWatcher.Wait returns on its very first poll
	// instead of blocking forever (no real hook script runs against
	// this fake).
	fileExists bool
	// captureErr, when set, is returned by CapturePane — failing the
	// idle detector's Wait mid-turn. killErr, when set, is returned by
	// KillSession — failing orchestrator.Complete. Both let a test fail
	// provisioning *after* the session launched (LOOM-63 review).
	captureErr error
	killErr    error
	// paneExit, when set, is called with a session's launch command and
	// returns what PaneExited reports for it — simulating that process
	// (an agent CLI that isn't installed, a provisioning script, a
	// one-shot command) having exited, LOOM-71. nil, or a nil return,
	// means "still running": every pre-LOOM-71 test's behavior.
	paneExit func(command string) *targets.PaneExit
	// runOnce, when set, replaces runOnceOutput/runOnceErr with a
	// per-command answer — e.g. an agent CLI probe that answers
	// differently before and after an install.
	runOnce func(command string) (string, error)
	// captureFunc, when set, replaces capture with a fresh value per call
	// (e.g. output that never goes quiet). onSendKeys, when set, runs
	// after every successful SendKeys (e.g. to cancel a request mid-turn).
	captureFunc func() string
	onSendKeys  func()
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
	if e.onSendKeys != nil {
		defer e.onSendKeys()
	}
	return nil
}

func (e *fakeExecutor) CapturePane(ctx context.Context, target string) (string, error) {
	if e.unreachable {
		return "", targets.ErrUnreachable
	}
	if e.captureErr != nil {
		return "", e.captureErr
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.sessions[target]; !ok {
		return "", fmt.Errorf("fakeExecutor: no such session %q", target)
	}
	if e.captureFunc != nil {
		return e.captureFunc(), nil
	}
	return e.capture, nil
}

func (e *fakeExecutor) KillSession(ctx context.Context, session string) error {
	if e.unreachable {
		return targets.ErrUnreachable
	}
	if e.killErr != nil {
		return e.killErr
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

// FileExists returns fileExists for every path — false by default,
// which preserves every existing test's behavior (they configure
// TierIdle via shortIdleAgentTypes and never poll FileExists at all).
// RemoveFile is a trivial stub; no test needs to observe its effect.
func (e *fakeExecutor) FileExists(ctx context.Context, path string) (bool, error) {
	if e.unreachable {
		return false, targets.ErrUnreachable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fileExists, nil
}
func (e *fakeExecutor) RemoveFile(ctx context.Context, path string) error { return nil }

func (e *fakeExecutor) RunOnce(ctx context.Context, command string) (string, error) {
	if e.unreachable {
		return "", targets.ErrUnreachable
	}
	if e.runOnce != nil {
		return e.runOnce(command)
	}
	return e.runOnceOutput, e.runOnceErr
}

func (e *fakeExecutor) PaneExited(ctx context.Context, target string) (*targets.PaneExit, error) {
	if e.unreachable {
		return nil, targets.ErrUnreachable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	sess, ok := e.sessions[target]
	if !ok {
		return nil, fmt.Errorf("fakeExecutor: no such session %q", target)
	}
	var exit *targets.PaneExit
	if e.paneExit != nil {
		exit = e.paneExit(sess.command)
	}
	if exit == nil && isProvisioning(sess.command) {
		// A provisioning recipe always exits (LOOM-90); unless a test
		// says otherwise it succeeds, reporting the directory it made.
		exit = &targets.PaneExit{Status: 0, Output: "loomux-workspace-path:" + fakeWorkspacePath(sess.command)}
	}
	return exit, nil
}

// isProvisioning reports whether command is a provisioning recipe.
func isProvisioning(command string) bool {
	return strings.HasPrefix(command, router.ProvisioningMarker)
}

// fakeWorkspacePath is where the fake "provisions" the workspace a recipe
// names: /fake-root/<name>.
func fakeWorkspacePath(recipe string) string {
	first, _, _ := strings.Cut(recipe, "\n")
	fields := strings.Fields(first)
	return "/fake-root/" + fields[len(fields)-1]
}

// launchedCommands lists every session's launch command, in no
// particular order.
func (e *fakeExecutor) launchedCommands() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, s := range e.sessions {
		out = append(out, s.command)
	}
	return out
}
