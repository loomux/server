// Package targets defines the TargetExecutor interface — the mechanism
// that runs tmux operations against a registry.Target, either locally or
// over SSH. See design spec §1.
package targets

import (
	"context"
	"errors"
	"fmt"

	"github.com/Loomux/server/registry"
)

// ErrUnreachable is returned when the target itself couldn't be reached
// (e.g. an SSH connection failure), as distinct from the requested tmux
// operation failing on an otherwise-reachable target.
var ErrUnreachable = errors.New("targets: target unreachable")

// TargetExecutor runs tmux operations against a single target. session and
// target parameters are plain tmux target-spec strings (a session name, or
// "session:window.pane") — this package doesn't model window/pane
// semantics beyond that; that belongs to the orchestrator.
type TargetExecutor interface {
	// NewSession creates a new detached tmux session. dir may be empty to
	// use the default working directory; command may be empty to start
	// the pane's default shell.
	NewSession(ctx context.Context, session, dir, command string) error

	// HasSession reports whether a session with the given name currently
	// exists. A nonexistent session is a normal (false, nil) result, not
	// an error.
	HasSession(ctx context.Context, session string) (bool, error)

	// SendKeys sends keys literally to target, optionally followed by
	// Enter.
	SendKeys(ctx context.Context, target, keys string, enter bool) error

	// CapturePane returns the current visible contents of target's pane.
	CapturePane(ctx context.Context, target string) (string, error)

	// KillSession terminates the named session.
	KillSession(ctx context.Context, session string) error

	// Close releases any resources held by the executor (e.g. an SSH
	// connection multiplexing master).
	Close() error
}

// NewExecutor constructs the TargetExecutor appropriate for t.Kind.
func NewExecutor(t *registry.Target) (TargetExecutor, error) {
	switch t.Kind {
	case registry.TargetKindLocal:
		return NewLocalExecutor(), nil
	case registry.TargetKindRemote:
		return NewRemoteExecutor(t.Host, t.User), nil
	default:
		return nil, fmt.Errorf("targets: unknown target kind %q", t.Kind)
	}
}
