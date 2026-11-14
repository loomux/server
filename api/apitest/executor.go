package apitest

import (
	"context"
	"fmt"
	"sync"

	"github.com/Loomux/server/targets"
)

// Executor is an in-memory targets.TargetExecutor: the one "machine"
// every target of a test server resolves to. It keeps sessions and what
// was sent to them, and runs nothing. A seeded task gets a session here,
// so what the server does to a task's pane can be read back.
type Executor struct {
	mu       sync.Mutex
	sessions map[string]*Session
	files    map[string]bool
	commands []string
}

// Session is one tmux session of an Executor, as it stands.
type Session struct {
	Name    string
	Dir     string
	Command string
	// Input is everything typed, pasted or sent as a named key into the
	// session, in order.
	Input []string
	// Pane is what CapturePane returns.
	Pane string
}

func newExecutor() *Executor {
	return &Executor{sessions: make(map[string]*Session), files: make(map[string]bool)}
}

var _ targets.TargetExecutor = (*Executor)(nil)

// Session returns a copy of the named session, or nil if there is none
// (never created, or killed).
func (e *Executor) Session(name string) *Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[name]
	if !ok {
		return nil
	}
	c := *s
	c.Input = append([]string(nil), s.Input...)
	return &c
}

// SetPane sets what the named session's pane shows.
func (e *Executor) SetPane(name, pane string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.sessions[name]; ok {
		s.Pane = pane
	}
}

// Commands returns every command RunOnce was given, in order.
func (e *Executor) Commands() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.commands...)
}

// TmuxSocket implements targets.TargetExecutor.
func (e *Executor) TmuxSocket() string { return TmuxSocket }

// NewSession implements targets.TargetExecutor.
func (e *Executor) NewSession(_ context.Context, session, dir, command string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.sessions[session]; ok {
		return fmt.Errorf("apitest: duplicate session: %s", session)
	}
	e.sessions[session] = &Session{Name: session, Dir: dir, Command: command}
	return nil
}

// PaneExited implements targets.TargetExecutor: a session's process
// never exits by itself here.
func (e *Executor) PaneExited(context.Context, string) (*targets.PaneExit, error) { return nil, nil }

// HasSession implements targets.TargetExecutor.
func (e *Executor) HasSession(_ context.Context, session string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.sessions[session]
	return ok, nil
}

func (e *Executor) input(target, what string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[target]
	if !ok {
		return fmt.Errorf("apitest: can't find session: %s", target)
	}
	s.Input = append(s.Input, what)
	return nil
}

// SendKeys implements targets.TargetExecutor.
func (e *Executor) SendKeys(_ context.Context, target, keys string, _ bool) error {
	return e.input(target, keys)
}

// PasteText implements targets.TargetExecutor.
func (e *Executor) PasteText(_ context.Context, target, text string, _ bool) error {
	if len(text) > targets.MaxPasteBytes {
		return targets.ErrTextTooLarge
	}
	return e.input(target, text)
}

// SendKey implements targets.TargetExecutor.
func (e *Executor) SendKey(_ context.Context, target, key string) error {
	return e.input(target, key)
}

// CapturePane implements targets.TargetExecutor.
func (e *Executor) CapturePane(_ context.Context, target string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[target]
	if !ok {
		return "", fmt.Errorf("apitest: can't find session: %s", target)
	}
	return s.Pane, nil
}

// KillSession implements targets.TargetExecutor.
func (e *Executor) KillSession(_ context.Context, session string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.sessions[session]; !ok {
		return fmt.Errorf("apitest: can't find session: %s", session)
	}
	delete(e.sessions, session)
	return nil
}

// Close implements targets.TargetExecutor.
func (e *Executor) Close() error { return nil }

// FileExists implements targets.TargetExecutor.
func (e *Executor) FileExists(_ context.Context, path string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.files[path], nil
}

// RemoveFile implements targets.TargetExecutor.
func (e *Executor) RemoveFile(_ context.Context, path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.files, path)
	return nil
}

// RunOnce implements targets.TargetExecutor: the command is recorded
// (see Commands), not run, and its output is empty.
func (e *Executor) RunOnce(_ context.Context, command string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commands = append(e.commands, command)
	return "", nil
}
